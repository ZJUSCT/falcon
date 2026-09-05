// Package config loads and validates the controller's single YAML
// configuration file (mounted from a ConfigMap at /etc/falcon/config.yaml).
// It holds the complete runtime configuration: the former Deployment flags
// (listen addresses, log level) and the behavior knobs
// (site identity, catalog, sync concurrency, publish route generation).
package config

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// GatewayRef points at the Gateway that terminates traffic for the publish
// hostnames. An empty Namespace means "same namespace as the controller".
type GatewayRef struct {
	Name        string `json:"name,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	SectionName string `json:"sectionName,omitempty"`
}

// PublishConfig describes the publishing topology: which Gateway and
// hostnames serve the mirrors, plus labels/annotations stamped onto every
// generated HTTPRoute. An empty Hostnames list disables publish-route
// generation (catalog/webapi keep working).
type PublishConfig struct {
	GatewayRef  GatewayRef        `json:"gatewayRef,omitempty"`
	Hostnames   []string          `json:"hostnames,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Config is the whole controller configuration file.
type Config struct {
	Auth struct {
		GitHub struct {
			ClientID       string  `json:"clientID,omitempty"`
			ClientSecret   string  `json:"clientSecret,omitempty"`
			AllowedUserIDs []int64 `json:"allowedUserIDs,omitempty"`
		} `json:"github,omitempty"`
	} `json:"auth,omitempty"`
	Admin struct {
		Host string `json:"host,omitempty"`
	} `json:"admin,omitempty"`
	Log struct {
		// Level is one of debug, info, warn, error (default info).
		Level string `json:"level,omitempty"`
	} `json:"log,omitempty"`

	API struct {
		MetricsBindAddress     string `json:"metricsBindAddress,omitempty"`
		HealthProbeBindAddress string `json:"healthProbeBindAddress,omitempty"`
		WebapiBindAddress      string `json:"webapiBindAddress,omitempty"`
	} `json:"api,omitempty"`

	Site struct {
		// URL is the fallback site URL (no trailing slash) used by
		// /mirrorz.json when the request Host is not in publish.hostnames.
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
	} `json:"site"`

	Catalog struct {
		// Enabled gates the GET /mirrorz.json endpoint.
		Enabled bool `json:"enabled,omitempty"`
	} `json:"catalog,omitempty"`

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
	cfg.API.WebapiBindAddress = ":8082"
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
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate normalizes and checks the config. It is called by Load; callers
// that build a Config programmatically (tests) should call it too.
func (c *Config) Validate() error {
	c.Site.URL = strings.TrimRight(strings.TrimSpace(c.Site.URL), "/")
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q is not one of debug, info, warn, error", c.Log.Level)
	}
	if c.Site.URL == "" {
		return fmt.Errorf("site.url must not be empty")
	}
	if c.Admin.Host != "" {
		c.Admin.Host = strings.TrimSpace(strings.ToLower(c.Admin.Host))
		if strings.ContainsAny(c.Admin.Host, "/:@?#") {
			return fmt.Errorf("admin.host must be a bare hostname")
		}
	}
	if !strings.Contains(c.Site.URL, "://") {
		return fmt.Errorf("site.url %q must carry a scheme (e.g. https://...)", c.Site.URL)
	}
	if len(c.Publish.Hostnames) > 0 && c.Publish.GatewayRef.Name == "" {
		return fmt.Errorf("publish.gatewayRef.name is required when publish.hostnames is set")
	}
	for _, host := range c.Publish.Hostnames {
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("publish.hostnames must not contain empty entries")
		}
		if strings.Contains(host, "/") {
			return fmt.Errorf("publish.hostnames entry %q must be a bare hostname", host)
		}
	}
	return nil
}

// PublishEnabled reports whether the controller should generate publish
// HTTPRoutes: it requires at least one publish hostname (a Gateway name is
// guaranteed alongside by Validate).
func (c *Config) PublishEnabled() bool {
	return len(c.Publish.Hostnames) > 0
}
