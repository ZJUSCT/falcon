package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/config"
	"github.com/ZJUSCT/falcon/internal/controller"
	"github.com/ZJUSCT/falcon/internal/webapi"
)

var scheme = runtime.NewScheme()

func main() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(snapshotv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(mirrorv1alpha1.AddToScheme(scheme))
	// The Deployment carries no business flags: everything lives in the
	// config file (mounted from the falcon-config ConfigMap).
	var configPath string
	flag.StringVar(&configPath, "config", "/etc/falcon/config.yaml", "Path to the controller configuration file (YAML).")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "falcon-controller: %v\n", err)
		os.Exit(1)
	}

	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		fmt.Fprintf(os.Stderr, "falcon-controller: invalid log level %q\n", cfg.Log.Level)
		os.Exit(1)
	}
	zapOptions := zap.Options{Development: false, Level: zapLevel}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	logger := ctrl.Log

	// POD_NAMESPACE scopes everything: the manager's cache (only objects of
	// this namespace are watched — the chart deploys one full stack per
	// namespace) and the zfs-agent usage aggregation (same-namespace
	// EndpointSlice discovery).
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		logger.Error(nil, "POD_NAMESPACE must be set to the controller's namespace")
		os.Exit(1)
	}

	if !cfg.PublishEnabled() {
		// Logged once at startup; the reconcilers then simply skip route
		// generation without touching existing HTTPRoutes.
		logger.Info("publish-route generation disabled: publish.hostnames is empty", "config", configPath)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{podNamespace: {}},
		},
		Metrics:                metricsserver.Options{BindAddress: cfg.API.MetricsBindAddress},
		HealthProbeBindAddress: cfg.API.HealthProbeBindAddress,
	})
	if err != nil {
		logger.Error(err, "unable to create controller manager")
		os.Exit(1)
	}

	// Node kubelet stats summary reader backing status.sizeBytes (best-effort
	// usage accounting of the active publish PVC, through the API server node
	// proxy — needs the chart's node-stats ClusterRole for nodes/proxy).
	// Summaries are cached per node for a minute so backfills across Mirrors
	// sharing a node do not hammer the proxy.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		logger.Error(err, "unable to create client-go client for node stats")
		os.Exit(1)
	}
	reconciler := &controller.MirrorReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("falcon-controller"), //nolint:staticcheck // migrate when typed Events support is ubiquitous
		Config:      cfg,
		SyncLimiter: controller.NewSyncLimiter(cfg.Sync.MaxConcurrent),
		UsageReader: controller.NewKubeletUsageReader(clientset, time.Minute),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to register Mirror controller")
		os.Exit(1)
	}

	proxyReconciler := &controller.ProxyMirrorReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("falcon-controller"), //nolint:staticcheck // migrate when typed Events support is ubiquitous
		Config:   cfg,
	}
	if err := proxyReconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to register ProxyMirror controller")
		os.Exit(1)
	}

	// Read-only mirrorz + legacy-compatible API listener (GET only, no auth:
	// everything it serves is public or spec-only data).
	if cfg.API.WebapiBindAddress != "0" {
		apiServer := &webapi.Server{
			Client:           mgr.GetClient(),
			Site: webapi.SiteConfig{URL: cfg.Site.URL, Abbr: cfg.Site.Abbr, Name: cfg.Site.Name, Logo: cfg.Site.Logo, LogoDarkmode: cfg.Site.LogoDarkmode, Homepage: cfg.Site.Homepage, Issue: cfg.Site.Issue, Request: cfg.Site.Request, Email: cfg.Site.Email, Group: cfg.Site.Group, Disk: cfg.Site.Disk, Note: cfg.Site.Note, Big: cfg.Site.Big, Disable: cfg.Site.Disable},
			PublishHostnames: cfg.Publish.Hostnames,
			CatalogEnabled:   cfg.Catalog.Enabled,
		}
		apiServer.Auth = &webapi.Authenticator{Config: webapi.GitHubAuthConfig{ClientID: cfg.Auth.GitHub.ClientID, ClientSecret: cfg.Auth.GitHub.ClientSecret, AllowedUserIDs: cfg.Auth.GitHub.AllowedUserIDs}, AdminHost: cfg.Admin.Host}
		apiServer.UIUpstream = os.Getenv("FALCON_UI_UPSTREAM")
		// ZFS usage aggregation behind GET /api/usage: enabled purely by the
		// ZFS_AGENT_SERVICE environment variable (the chart injects it when
		// zfsAgent.enabled); unset leaves the endpoint a 404.
		if agentService := os.Getenv("ZFS_AGENT_SERVICE"); agentService != "" {
			apiServer.Usage = webapi.NewUsageAggregator(clientset, podNamespace, agentService)
			logger.Info("zfs-agent usage aggregation enabled", "service", agentService, "namespace", podNamespace)
		}
		httpSrv := &http.Server{
			Addr:              cfg.API.WebapiBindAddress,
			Handler:           apiServer.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		if err := mgr.Add(managerRunnable(httpSrv)); err != nil {
			logger.Error(err, "unable to register mirrorz API server")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to register health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to register readiness check")
		os.Exit(1)
	}

	logger.Info("starting Falcon controller manager",
		"namespace", podNamespace,
		"maxConcurrentSyncs", cfg.Sync.MaxConcurrent,
		"publishRoutes", cfg.PublishEnabled())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "controller manager exited")
		os.Exit(1)
	}
}

// managerRunnable wraps an http.Server as a controller-runtime Runnable:
// it serves until the manager context is cancelled, then shuts down
// gracefully.
func managerRunnable(srv *http.Server) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		errCh := make(chan error, 1)
		go func() {
			ctrl.Log.Info("starting read-only mirrorz API server", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctrl.Log.Info("shutting down read-only mirrorz API server", "addr", srv.Addr)
			return srv.Shutdown(shutdownCtx)
		}
	})
}
