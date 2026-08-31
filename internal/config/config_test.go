package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadFullConfig(t *testing.T) {
	path := writeTemp(t, `
log:
  level: debug
leaderElection:
  enabled: true
api:
  metricsBindAddress: ":9080"
  healthProbeBindAddress: ":9081"
  webapiBindAddress: ":9082"
site:
  url: https://mirrors.zjusct.io/
  abbr: ZJU
  name: Zhejiang University Mirror
catalog:
  enabled: true
sync:
  maxConcurrent: 4
serving:
  gatewayRef:
    name: nginx-gateway
    namespace: ""
    sectionName: https
  hostnames:
    - mirrors.zjusct.io
    - mirror.zju.edu.cn
  labels:
    app: mirrors
  annotations:
    cert: managed
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want debug", cfg.Log.Level)
	}
	if !cfg.LeaderElection.Enabled {
		t.Errorf("leaderElection.enabled = false, want true")
	}
	if cfg.API.MetricsBindAddress != ":9080" || cfg.API.HealthProbeBindAddress != ":9081" || cfg.API.WebapiBindAddress != ":9082" {
		t.Errorf("api addresses wrong: %+v", cfg.API)
	}
	if cfg.Site.URL != "https://mirrors.zjusct.io" { // trailing slash trimmed
		t.Errorf("site.url = %q, want trailing slash trimmed", cfg.Site.URL)
	}
	if cfg.Site.Abbr != "ZJU" || cfg.Site.Name != "Zhejiang University Mirror" {
		t.Errorf("site identity wrong: %+v", cfg.Site)
	}
	if !cfg.Catalog.Enabled {
		t.Errorf("catalog.enabled = false, want true")
	}
	if cfg.Sync.MaxConcurrent != 4 {
		t.Errorf("sync.maxConcurrent = %d, want 4", cfg.Sync.MaxConcurrent)
	}
	if !cfg.ServingEnabled() {
		t.Errorf("ServingEnabled() = false, want true")
	}
	if cfg.Serving.GatewayRef.Name != "nginx-gateway" || cfg.Serving.GatewayRef.Namespace != "" || cfg.Serving.GatewayRef.SectionName != "https" {
		t.Errorf("gatewayRef wrong: %+v", cfg.Serving.GatewayRef)
	}
	if len(cfg.Serving.Hostnames) != 2 || cfg.Serving.Hostnames[1] != "mirror.zju.edu.cn" {
		t.Errorf("hostnames wrong: %v", cfg.Serving.Hostnames)
	}
	if cfg.Serving.Labels["app"] != "mirrors" || cfg.Serving.Annotations["cert"] != "managed" {
		t.Errorf("labels/annotations wrong: %v %v", cfg.Serving.Labels, cfg.Serving.Annotations)
	}
}

func TestLoadDefaultsForSparseConfig(t *testing.T) {
	path := writeTemp(t, "site:\n  url: https://mirrors.example.com\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level default = %q, want info", cfg.Log.Level)
	}
	if cfg.API.MetricsBindAddress != ":8080" || cfg.API.HealthProbeBindAddress != ":8081" || cfg.API.WebapiBindAddress != ":8082" {
		t.Errorf("api address defaults wrong: %+v", cfg.API)
	}
	if cfg.LeaderElection.Enabled {
		t.Errorf("leaderElection default = true, want false")
	}
	if cfg.Catalog.Enabled {
		t.Errorf("catalog default = enabled, want disabled")
	}
	if cfg.Sync.MaxConcurrent != 0 {
		t.Errorf("sync.maxConcurrent default = %d, want 0 (unlimited)", cfg.Sync.MaxConcurrent)
	}
	if cfg.ServingEnabled() {
		t.Errorf("ServingEnabled() = true for empty hostnames, want false")
	}
}

func TestLoadInvalidConfigs(t *testing.T) {
	cases := map[string]string{
		"missing site.url":        "catalog:\n  enabled: true\n",
		"site.url without scheme": "site:\n  url: mirrors.example.com\n",
		"bad log level":           "site:\n  url: https://a\nlog:\n  level: verbose\n",
		"gatewayRef without name": "site:\n  url: https://a\nserving:\n  hostnames: [mirrors.example.com]\n",
		"empty hostname entry":    "site:\n  url: https://a\nserving:\n  gatewayRef:\n    name: gw\n  hostnames: [\" \"]\n",
		"hostname with path":      "site:\n  url: https://a\nserving:\n  gatewayRef:\n    name: gw\n  hostnames: [mirrors.example.com/foo]\n",
		"not YAML":                "\t\tbroken: [",
	}
	for name, content := range cases {
		path := writeTemp(t, content)
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load succeeded, want error", name)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load of missing file succeeded, want error")
	}
}
