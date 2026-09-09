// Command zfs-agent is a read-only ZFS usage reporter. It runs as one
// DaemonSet pod per storage node, executes the host's zfs/zpool binaries
// (chrooted into the /host hostPath mount), and answers:
//
//   - GET /v1/zfs   the node's ZFS dataset/snapshot usage report (served from
//     a background-refreshed cache; see internal/zfsagent/refresher.go)
//   - GET /healthz  liveness/readiness probe
//
// It never talks to the Kubernetes API; the controller's webapi discovers the
// agents through the headless Service and aggregates their reports (see
// internal/webapi/usage.go). The listen port (9474) is part of that contract
// and therefore not configurable.
//
// When OTEL_EXPORTER_OTLP_ENDPOINT is set (chart value zfsAgent.otelEndpoint),
// the agent additionally collects ZFS performance counters every 15s and
// pushes them as OTLP metrics to that endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ZJUSCT/falcon/internal/zfsagent"
)

const defaultBind = ":9474"

// hostRoot is where the DaemonSet mounts the host's root filesystem. When it
// exists the agent chroots every zfs/zpool exec into it; otherwise (local
// development) the local binaries run directly.
const hostRoot = "/host"

func main() {
	var bind, zfsBin, pools, nodeName string
	flag.StringVar(&bind, "bind", defaultBind, "Listen address for the HTTP endpoints.")
	flag.StringVar(&zfsBin, "zfs-bin", "", fmt.Sprintf("Path to the zfs binary (%s). Its directory also provides zpool.", strings.Join(zfsagent.DefaultZfsBinCandidates, ", ")))
	flag.StringVar(&pools, "pools", "", "Comma-separated ZFS pools to report (default: all pools, via zpool list).")
	flag.StringVar(&nodeName, "node-name", "", "Node name reported in /v1/zfs (default: $NODE_NAME, then $HOSTNAME).")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if nodeName == "" {
		// The chart injects the Kubernetes node name via the downward API;
		// $HOSTNAME in a pod is the pod name, the last resort before the
		// container's hostname.
		nodeName = os.Getenv("NODE_NAME")
		if nodeName == "" {
			nodeName = os.Getenv("HOSTNAME")
			if nodeName == "" {
				name, err := os.Hostname()
				if err != nil {
					logger.Error("cannot determine node name", "error", err.Error())
					os.Exit(1)
				}
				nodeName = name
			}
		}
	}

	var poolList []string
	for _, pool := range strings.Split(pools, ",") {
		if pool = strings.TrimSpace(pool); pool != "" {
			poolList = append(poolList, pool)
		}
	}

	var candidates []string
	if zfsBin != "" {
		candidates = []string{zfsBin}
	}

	root := ""
	if info, err := os.Stat(hostRoot); err == nil && info.IsDir() {
		// Chroot into the host root: the zfs exec then sees the host's
		// /dev, /sys and module state and links against the host's
		// libraries. Requires a privileged (root) container.
		root = hostRoot
	}

	collector := zfsagent.NewCollector(nodeName, poolList, zfsagent.NewHostRunner(root, candidates))
	collector.Log = logger

	// The push pipeline is built before any goroutine starts, so a
	// construction failure can simply exit. The exporter reads the standard
	// OTEL_EXPORTER_OTLP_* environment variables.
	var pusher *zfsagent.OTLPPusher
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		var err error
		if pusher, err = zfsagent.NewOTLPPusher(context.Background(), nodeName, logger); err != nil {
			logger.Error("cannot create OTLP pusher", "endpoint", endpoint, "error", err.Error())
			os.Exit(1)
		}
		logger.Info("OTLP metric push enabled", "endpoint", endpoint, "interval", zfsagent.PushInterval.String())
	}

	// Reports are served from the refresher's cache; until the first sweep
	// completes (it starts immediately), /v1/zfs answers 500 briefly.
	refresher := zfsagent.NewRefresher(collector)
	go refresher.Run(context.Background())
	if pusher != nil {
		go pushPerfLoop(collector, refresher, pusher)
	}

	server := &http.Server{
		Addr:              bind,
		Handler:           (&zfsagent.Server{Refresher: refresher}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("starting zfs-agent",
		"bind", bind,
		"node", nodeName,
		"hostRoot", root,
		"pools", poolsOrAll(pools),
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("zfs-agent exited", "error", err.Error())
		os.Exit(1)
	}
}

// pushPerfLoop collects one PerfSample per push interval and hands it to the
// pusher, with the dataset→PVC index rebuilt from the latest cached report
// (small: one entry per dataset). Persistent iostat processes sample contiguous
// windows; the first framed window arrives after two intervals. Push/export
// failures are logged by the OTLP error handler and never stop the loop.
func pushPerfLoop(collector *zfsagent.Collector, refresher *zfsagent.Refresher, pusher *zfsagent.OTLPPusher) {
	defer collector.ClosePerf()
	ticker := time.NewTicker(zfsagent.PushInterval)
	defer ticker.Stop()
	collect := func() {
		sample := collector.CollectPerf(context.Background())
		index := pvcIndex(refresher)
		pusher.Push(sample, func(_, dataset string) string {
			return index[dataset]
		})
	}
	collect()
	for range ticker.C {
		collect()
	}
}

// pvcIndex maps dataset names to "namespace/name" PVC references from the
// latest report (nil when no report exists yet — lookups then return "").
// Dataset names are pool-qualified, so no pool key is needed.
func pvcIndex(refresher *zfsagent.Refresher) map[string]string {
	report, err := refresher.Snapshot()
	if err != nil {
		return nil
	}
	index := make(map[string]string, 16)
	for _, pool := range report.Pools {
		for _, ds := range pool.Datasets {
			if ds.PVC != nil {
				index[ds.Name] = ds.PVC.Namespace + "/" + ds.PVC.Name
			}
		}
	}
	return index
}

func poolsOrAll(pools string) string {
	if pools == "" {
		return "<all>"
	}
	return pools
}
