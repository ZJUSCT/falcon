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
  webapiBindAddress: ":9082"
site:
  url: https://mirrors.zjusct.io/
  abbr: ZJU
  name: Zhejiang University Mirror
catalog:
  enabled: true
sync:
  maxConcurrent: 4
publish:
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
	if !cfg.PublishEnabled() {
		t.Errorf("PublishEnabled() = false, want true")
	}
	if cfg.Publish.GatewayRef.Name != "nginx-gateway" || cfg.Publish.GatewayRef.Namespace != "" || cfg.Publish.GatewayRef.SectionName != "https" {
		t.Errorf("gatewayRef wrong: %+v", cfg.Publish.GatewayRef)
	}
	if len(cfg.Publish.Hostnames) != 2 || cfg.Publish.Hostnames[1] != "mirror.zju.edu.cn" {
		t.Errorf("hostnames wrong: %v", cfg.Publish.Hostnames)
	}
	if cfg.Publish.Labels["app"] != "mirrors" || cfg.Publish.Annotations["cert"] != "managed" {
		t.Errorf("labels/annotations wrong: %v %v", cfg.Publish.Labels, cfg.Publish.Annotations)
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
	if cfg.Catalog.Enabled {
		t.Errorf("catalog default = enabled, want disabled")
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
		"missing site.url":        "catalog:\n  enabled: true\n",
		"site.url without scheme": "site:\n  url: mirrors.example.com\n",
		"bad log level":           "site:\n  url: https://a\nlog:\n  level: verbose\n",
		"gatewayRef without name": "site:\n  url: https://a\npublish:\n  hostnames: [mirrors.example.com]\n",
		"empty hostname entry":    "site:\n  url: https://a\npublish:\n  gatewayRef:\n    name: gw\n  hostnames: [\" \"]\n",
		"hostname with path":      "site:\n  url: https://a\npublish:\n  gatewayRef:\n    name: gw\n  hostnames: [mirrors.example.com/foo]\n",
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
site:
  url: https://${FALCON_TEST_HOST}/
auth:
  github:
    clientID: ${FALCON_TEST_CLIENT}
    clientSecret: ${FALCON_TEST_SECRET}
    allowedUserIDs: [9007199254740993]
publish:
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
	if cfg.Auth.GitHub.ClientSecret != secret {
		t.Fatal("clientSecret was not preserved exactly")
	}
	if cfg.Auth.GitHub.ClientID != "client-id" || cfg.Site.URL != "https://mirrors.example.org" {
		t.Fatal("nested string expansion or subsequent normalization failed")
	}
	if len(cfg.Publish.Hostnames) != 1 || cfg.Publish.Hostnames[0] != "mirrors.example.org" {
		t.Fatal("string list expansion failed")
	}
	if cfg.Publish.Annotations["${UNCHANGED_KEY}"] != "client-id/mirrors.example.org" {
		t.Fatal("map value expansion changed the key or failed to expand the value")
	}
	if len(cfg.Auth.GitHub.AllowedUserIDs) != 1 || cfg.Auth.GitHub.AllowedUserIDs[0] != 9007199254740993 {
		t.Fatal("integer configuration lost precision")
	}
}

func TestLoadEnvironmentLiterals(t *testing.T) {
	t.Setenv("FALCON_TEST_VALUE", "expanded")
	path := writeTemp(t, `
site:
  url: https://mirrors.example.org
  note: '$HOME $request_uri $1 $$ $ $${FALCON_TEST_VALUE} $${UNSET:-default} ${FALCON_TEST_VALUE}'
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "$HOME $request_uri $1 $$ $ ${FALCON_TEST_VALUE} ${UNSET:-default} expanded"
	if cfg.Site.Note != want {
		t.Errorf("site.note = %q, want %q", cfg.Site.Note, want)
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
			path := writeTemp(t, "site:\n  url: https://mirrors.example.org\nauth:\n  github:\n    clientSecret: '"+tc.input+"'\n")
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "auth.github.clientSecret: "+tc.want) {
				t.Fatalf("Load error = %v, want field path and %q", err, tc.want)
			}
		})
	}
}

func TestLoadEnvironmentValidationDoesNotExposeValues(t *testing.T) {
	t.Setenv("FALCON_TEST_SENSITIVE", "sensitive/invalid-value")
	cases := map[string]string{
		"log level": "site:\n  url: https://mirrors.example.org\nlog:\n  level: ${FALCON_TEST_SENSITIVE}\n",
		"site URL":  "site:\n  url: ${FALCON_TEST_SENSITIVE}\n",
		"hostname":  "site:\n  url: https://mirrors.example.org\npublish:\n  gatewayRef:\n    name: gw\n  hostnames: ['${FALCON_TEST_SENSITIVE}']\n",
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
