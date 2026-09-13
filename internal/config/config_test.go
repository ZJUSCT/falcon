package config

import (
	"os"
	"path/filepath"
	"strings"
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
api:
  metricsBindAddress: ":9080"
  healthProbeBindAddress: ":9081"
  mirrorzBindAddress: ":9082"
  adminBindAddress: ":9083"
mirrorz:
  enabled: true
  site:
    url: https://mirrors.zjusct.io/
    abbr: ZJU
    name: Zhejiang University Mirror
sync:
  maxConcurrent: 4
admin:
  enabled: true
  host: mirrors-admin.zjusct.io
  oauth:
    clientID: client-id
    clientSecret: secret
    allowedUserIDs: [1, 2]
publish:
  http:
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
	if cfg.API.MetricsBindAddress != ":9080" || cfg.API.HealthProbeBindAddress != ":9081" || cfg.API.MirrorzBindAddress != ":9082" || cfg.API.AdminBindAddress != ":9083" {
		t.Errorf("api addresses wrong: %+v", cfg.API)
	}
	if cfg.Mirrorz.Site.URL != "https://mirrors.zjusct.io" { // trailing slash trimmed
		t.Errorf("mirrorz.site.url = %q, want trailing slash trimmed", cfg.Mirrorz.Site.URL)
	}
	if cfg.Mirrorz.Site.Abbr != "ZJU" || cfg.Mirrorz.Site.Name != "Zhejiang University Mirror" {
		t.Errorf("mirrorz.site identity wrong: %+v", cfg.Mirrorz.Site)
	}
	if !cfg.Mirrorz.Enabled {
		t.Errorf("mirrorz.enabled = false, want true")
	}
	if cfg.Sync.MaxConcurrent != 4 {
		t.Errorf("sync.maxConcurrent = %d, want 4", cfg.Sync.MaxConcurrent)
	}
	if !cfg.Admin.Enabled || cfg.Admin.Host != "mirrors-admin.zjusct.io" {
		t.Errorf("admin wrong: %+v", cfg.Admin)
	}
	if cfg.Admin.OAuth.ClientID != "client-id" || len(cfg.Admin.OAuth.AllowedUserIDs) != 2 {
		t.Errorf("admin.oauth wrong: %+v", cfg.Admin.OAuth)
	}
	if !cfg.PublishEnabled() {
		t.Errorf("PublishEnabled() = false, want true")
	}
	if cfg.Publish.HTTP.GatewayRef.Name != "nginx-gateway" || cfg.Publish.HTTP.GatewayRef.Namespace != "" || cfg.Publish.HTTP.GatewayRef.SectionName != "https" {
		t.Errorf("gatewayRef wrong: %+v", cfg.Publish.HTTP.GatewayRef)
	}
	if len(cfg.Publish.HTTP.Hostnames) != 2 || cfg.Publish.HTTP.Hostnames[1] != "mirror.zju.edu.cn" {
		t.Errorf("hostnames wrong: %v", cfg.Publish.HTTP.Hostnames)
	}
	if cfg.Publish.HTTP.Labels["app"] != "mirrors" || cfg.Publish.HTTP.Annotations["cert"] != "managed" {
		t.Errorf("labels/annotations wrong: %v %v", cfg.Publish.HTTP.Labels, cfg.Publish.HTTP.Annotations)
	}
}

func TestLoadDefaultsForSparseConfig(t *testing.T) {
	path := writeTemp(t, "mirrorz:\n  site:\n    url: https://mirrors.example.com\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level default = %q, want info", cfg.Log.Level)
	}
	if cfg.API.MetricsBindAddress != ":8080" || cfg.API.HealthProbeBindAddress != ":8081" || cfg.API.MirrorzBindAddress != ":8082" || cfg.API.AdminBindAddress != ":8083" {
		t.Errorf("api address defaults wrong: %+v", cfg.API)
	}
	if cfg.Mirrorz.Enabled {
		t.Errorf("mirrorz.enabled default = true, want false")
	}
	if cfg.Sync.MaxConcurrent != 0 {
		t.Errorf("sync.maxConcurrent default = %d, want 0 (unlimited)", cfg.Sync.MaxConcurrent)
	}
	if cfg.PublishEnabled() {
		t.Errorf("PublishEnabled() = true for empty hostnames, want false")
	}
}

func TestLoadInvalidConfigs(t *testing.T) {
	cases := map[string]string{
		"missing site.url":        "mirrorz:\n  enabled: true\n",
		"site.url without scheme": "mirrorz:\n  enabled: true\n  site:\n    url: mirrors.example.com\n",
		"bad log level":           "log:\n  level: verbose\n",
		"gatewayRef without name": "publish:\n  http:\n    hostnames: [mirrors.example.com]\n",
		"empty hostname entry":    "publish:\n  http:\n    gatewayRef:\n      name: gw\n    hostnames: [\" \"]\n",
		"hostname with path":      "publish:\n  http:\n    gatewayRef:\n      name: gw\n    hostnames: [mirrors.example.com/foo]\n",
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

func TestLoadEnvironment(t *testing.T) {
	// 故意包含 YAML 特殊字符和占位符：这些内容必须原样进入字段，不能再次解析或替换。
	secret := "'\"\\\n: {injected: true} # $HOME ${FALCON_TEST_UNSET} $${LITERAL}"
	t.Setenv("FALCON_TEST_SECRET", secret)
	t.Setenv("FALCON_TEST_HOST", "mirrors.example.org")
	t.Setenv("FALCON_TEST_CLIENT", "client-id")
	path := writeTemp(t, `
mirrorz:
  site:
    url: https://${FALCON_TEST_HOST}/
admin:
  enabled: true
  host: ${FALCON_TEST_HOST}
  oauth:
    clientID: ${FALCON_TEST_CLIENT}
    clientSecret: ${FALCON_TEST_SECRET}
    allowedUserIDs: [9007199254740993]
publish:
  http:
    gatewayRef:
      name: gateway
    hostnames: ["${FALCON_TEST_HOST}"]
    annotations:
      '${UNCHANGED_KEY}': '${FALCON_TEST_CLIENT}/${FALCON_TEST_HOST}'
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.OAuth.ClientSecret != secret {
		t.Fatal("clientSecret was not preserved exactly")
	}
	if cfg.Admin.OAuth.ClientID != "client-id" || cfg.Mirrorz.Site.URL != "https://mirrors.example.org" {
		t.Fatal("nested string expansion or subsequent normalization failed")
	}
	if len(cfg.Publish.HTTP.Hostnames) != 1 || cfg.Publish.HTTP.Hostnames[0] != "mirrors.example.org" {
		t.Fatal("string list expansion failed")
	}
	if cfg.Publish.HTTP.Annotations["${UNCHANGED_KEY}"] != "client-id/mirrors.example.org" {
		t.Fatal("map value expansion changed the key or failed to expand the value")
	}
	if len(cfg.Admin.OAuth.AllowedUserIDs) != 1 || cfg.Admin.OAuth.AllowedUserIDs[0] != 9007199254740993 {
		t.Fatal("integer configuration lost precision")
	}
}

func TestLoadEnvironmentLiterals(t *testing.T) {
	t.Setenv("FALCON_TEST_VALUE", "expanded")
	path := writeTemp(t, `
mirrorz:
  site:
    url: https://mirrors.example.org
    note: '$HOME $request_uri $1 $$ $ $${FALCON_TEST_VALUE} $${UNSET:-default} ${FALCON_TEST_VALUE}'
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "$HOME $request_uri $1 $$ $ ${FALCON_TEST_VALUE} ${UNSET:-default} expanded"
	if cfg.Mirrorz.Site.Note != want {
		t.Errorf("mirrorz.site.note = %q, want %q", cfg.Mirrorz.Site.Note, want)
	}
}

func TestLoadEnvironmentErrors(t *testing.T) {
	t.Setenv("FALCON_TEST_EMPTY", "")
	t.Setenv("FALCON_TEST_MISSING", "")
	if err := os.Unsetenv("FALCON_TEST_MISSING"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		input string
		want  string
	}{
		"missing":      {"${FALCON_TEST_MISSING}", `environment variable "FALCON_TEST_MISSING" must be set and non-empty`},
		"empty":        {"${FALCON_TEST_EMPTY}", `environment variable "FALCON_TEST_EMPTY" must be set and non-empty`},
		"unclosed":     {"${FALCON_TEST_MISSING", "unterminated environment placeholder"},
		"empty name":   {"${}", "environment placeholder must use ${NAME}"},
		"invalid name": {"${BAD-NAME}", "environment placeholder must use ${NAME}"},
		"numeric name": {"${1}", "environment placeholder must use ${NAME}"},
		"shell default": {"${FALCON_TEST_EMPTY:-fallback}",
			"environment placeholder must use ${NAME}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, "mirrorz:\n  site:\n    url: https://mirrors.example.org\nadmin:\n  enabled: true\n  host: mirrors.example.org\n  oauth:\n    clientSecret: '"+tc.input+"'\n")
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "admin.oauth.clientSecret: "+tc.want) {
				t.Fatalf("Load error = %v, want field path and %q", err, tc.want)
			}
		})
	}
}

func TestLoadEnvironmentValidationDoesNotExposeValues(t *testing.T) {
	t.Setenv("FALCON_TEST_SENSITIVE", "sensitive/invalid-value")
	cases := map[string]string{
		"log level": "log:\n  level: ${FALCON_TEST_SENSITIVE}\n",
		"site URL":  "mirrorz:\n  enabled: true\n  site:\n    url: ${FALCON_TEST_SENSITIVE}\n",
		"hostname":  "publish:\n  http:\n    gatewayRef:\n      name: gw\n    hostnames: ['${FALCON_TEST_SENSITIVE}']\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeTemp(t, content))
			if err == nil {
				t.Fatal("Load succeeded with invalid expanded configuration")
			}
			if strings.Contains(err.Error(), "sensitive/invalid-value") {
				t.Fatal("validation error exposed an environment value")
			}
		})
	}
}
