// Package config loads and validates the controller's single YAML
// configuration file (mounted from a ConfigMap at /etc/falcon/config.yaml).
// It holds the complete runtime configuration: the former Deployment flags
// (listen addresses, log level) and the behavior knobs
// (mirrorz output, sync concurrency, per-protocol publish settings).
package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"
)

// GatewayRef points at the Gateway that terminates traffic for a publish
// protocol. An empty Namespace means "same namespace as the controller".
type GatewayRef struct {
	Name        string `json:"name,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	SectionName string `json:"sectionName,omitempty"`
}

// HTTPPublishConfig describes the HTTP publishing topology: which Gateway
// and hostnames serve the mirrors, plus labels/annotations stamped onto
// every generated HTTPRoute. An empty Hostnames list disables publish-route
// generation (catalog/webapi keep working).
type HTTPPublishConfig struct {
	GatewayRef  GatewayRef        `json:"gatewayRef,omitempty"`
	Hostnames   []string          `json:"hostnames,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PublishConfig scopes publishing per protocol; each protocol names its own
// Gateway and hostnames because they may be served on different domains.
// Additional protocols (e.g. an rsync gateway) will live alongside HTTP.
type PublishConfig struct {
	HTTP HTTPPublishConfig `json:"http,omitempty"`
}

// SiteConfig is the identity of this mirror site, rendered into the site
// section of the mirrorz document.
type SiteConfig struct {
	// URL is the fallback site URL (no trailing slash) used by
	// /mirrorz.json when the request Host is not in publish.http.hostnames.
	URL          string `json:"url"`
	Abbr         string `json:"abbr,omitempty"`
	Name         string `json:"name,omitempty"`
	Logo         string `json:"logo,omitempty"`
	LogoDarkmode string `json:"logo_darkmode,omitempty"`
	Homepage     string `json:"homepage,omitempty"`
	Issue        string `json:"issue,omitempty"`
	Request      string `json:"request,omitempty"`
	Email        string `json:"email,omitempty"`
	Group        string `json:"group,omitempty"`
	Disk         string `json:"disk,omitempty"`
	Note         string `json:"note,omitempty"`
	Big          string `json:"big,omitempty"`
	Disable      bool   `json:"disable,omitempty"`
}

// MirrorzConfig groups everything driving the /mirrorz.json output: the
// endpoint switch and the site identity the document carries. The document
// follows the MirrorZ spec, hence the name.
type MirrorzConfig struct {
	// Enabled gates the GET /mirrorz.json endpoint.
	Enabled bool       `json:"enabled,omitempty"`
	Site    SiteConfig `json:"site"`
}

// AdminOAuthConfig is the GitHub OAuth application guarding the whole admin
// surface when configured: every admin endpoint then requires a session.
// Without it the admin endpoints answer unauthenticated (debug/local mode).
type AdminOAuthConfig struct {
	ClientID       string  `json:"clientID,omitempty"`
	ClientSecret   string  `json:"clientSecret,omitempty"`
	AllowedUserIDs []int64 `json:"allowedUserIDs,omitempty"`
}

// Config is the whole controller configuration file.
type Config struct {
	Admin struct {
		// Enabled turns on the admin listener and its API/UI surface.
		Enabled bool `json:"enabled,omitempty"`
		// Host is the admin hostname: the OAuth redirect anchor and the
		// hostname the UI is served on. Required when OAuth is configured.
		Host string `json:"host,omitempty"`
		// OAuth gates the admin surface on GitHub login. The whole
		// configuration is optional; a clientSecret of "${...}" expands
		// from the environment (the chart injects a Secret).
		OAuth AdminOAuthConfig `json:"oauth,omitempty"`
	} `json:"admin,omitempty"`
	Log struct {
		// Level is one of debug, info, warn, error (default info).
		Level string `json:"level,omitempty"`
	} `json:"log,omitempty"`

	API struct {
		MetricsBindAddress     string `json:"metricsBindAddress,omitempty"`
		HealthProbeBindAddress string `json:"healthProbeBindAddress,omitempty"`
		// MirrorzBindAddress serves the public catalog port: /mirrorz.json
		// only. "0" disables the listener.
		MirrorzBindAddress string `json:"mirrorzBindAddress,omitempty"`
		// AdminBindAddress serves the admin port: /api, OAuth and the UI
		// reverse proxy. "0" disables the listener.
		AdminBindAddress string `json:"adminBindAddress,omitempty"`
	} `json:"api,omitempty"`

	Mirrorz MirrorzConfig `json:"mirrorz,omitempty"`

	Sync struct {
		// MaxConcurrent caps the number of concurrently running sync Jobs
		// across all Mirrors. <= 0 means unlimited.
		MaxConcurrent int `json:"maxConcurrent,omitempty"`
	} `json:"sync,omitempty"`

	Publish PublishConfig `json:"publish,omitempty"`
}

// Default returns a Config filled with the built-in defaults. Load applies the
// same defaults after decoding, so a sparse file yields a usable config.
const defaultLogLevel = "info"

func Default() *Config {
	cfg := &Config{}
	cfg.Log.Level = defaultLogLevel
	cfg.API.MetricsBindAddress = ":8080"
	cfg.API.HealthProbeBindAddress = ":8081"
	cfg.API.MirrorzBindAddress = ":8082"
	cfg.API.AdminBindAddress = ":8083"
	return cfg
}

// Load reads and validates the YAML configuration file at path. Invalid
// configuration is a hard error: the controller refuses to start.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := expandConfigEnv(reflect.ValueOf(cfg).Elem(), ""); err != nil {
		return nil, fmt.Errorf("expand config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate normalizes and checks the config. It is called by Load; callers
// that build a Config programmatically (tests) should call it too.
func (c *Config) Validate() error {
	c.Mirrorz.Site.URL = strings.TrimRight(strings.TrimSpace(c.Mirrorz.Site.URL), "/")
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of debug, info, warn, error")
	}
	// The site identity exists only for the mirrorz document, so it is
	// required only when the endpoint is on.
	if c.Mirrorz.Enabled {
		if c.Mirrorz.Site.URL == "" {
			return fmt.Errorf("mirrorz.site.url must not be empty when mirrorz.enabled is set")
		}
		if !strings.Contains(c.Mirrorz.Site.URL, "://") {
			return fmt.Errorf("mirrorz.site.url must carry a scheme (e.g. https://...)")
		}
	}
	if c.Admin.Host != "" {
		c.Admin.Host = strings.TrimSpace(strings.ToLower(c.Admin.Host))
		if strings.ContainsAny(c.Admin.Host, "/:@?#") {
			return fmt.Errorf("admin.host must be a bare hostname")
		}
	}
	if c.Admin.OAuth.ClientID != "" {
		if !c.Admin.Enabled {
			return fmt.Errorf("admin.oauth requires admin.enabled")
		}
		if c.Admin.Host == "" {
			return fmt.Errorf("admin.host must not be empty when admin.oauth is configured")
		}
	}
	http := c.Publish.HTTP
	if len(http.Hostnames) > 0 && http.GatewayRef.Name == "" {
		return fmt.Errorf("publish.http.gatewayRef.name is required when publish.http.hostnames is set")
	}
	for _, host := range http.Hostnames {
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("publish.http.hostnames must not contain empty entries")
		}
		if strings.Contains(host, "/") {
			return fmt.Errorf("publish.http.hostnames entries must be bare hostnames")
		}
	}
	return nil
}

// PublishEnabled reports whether the controller should generate publish
// HTTPRoutes: it requires at least one HTTP publish hostname (a Gateway
// name is guaranteed alongside by Validate).
func (c *Config) PublishEnabled() bool {
	return len(c.Publish.HTTP.Hostnames) > 0
}
