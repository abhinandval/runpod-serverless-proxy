// Package config loads the gateway's YAML configuration: the gateway itself,
// the fleet credentials, the units (one serverless endpoint each) and the
// models clients ask for.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole configuration file.
type Config struct {
	Gateway Gateway          `yaml:"gateway"`
	Fleet   Fleet            `yaml:"fleet"`
	Units   map[string]Unit  `yaml:"units"`
	Models  map[string]Model `yaml:"models"`
}

// Gateway configures the client-facing server.
type Gateway struct {
	Listen   string   `yaml:"listen"`
	APIKeys  []string `yaml:"api_keys"`
	Database string   `yaml:"database"`
}

// Fleet holds the provider credentials and ownership settings.
type Fleet struct {
	Provider      string `yaml:"provider"`
	ManagementKey string `yaml:"management_key"`
	InferenceKey  string `yaml:"inference_key"`
	HFToken       string `yaml:"hf_token"`
	NamePrefix    string `yaml:"name_prefix"`
}

// Unit is one serverless endpoint running one engine with one model.
type Unit struct {
	Engine        string `yaml:"engine"`
	EngineVersion string `yaml:"engine_version"`
	// EndpointID is the RunPod endpoint serving this unit. The fleet reconciler
	// will fill it in; until then it is set by hand.
	EndpointID string `yaml:"endpoint_id"`
	// URL overrides the address derived from EndpointID (local testing).
	URL        string         `yaml:"url"`
	Model      map[string]any `yaml:"model"`
	EngineArgs map[string]any `yaml:"engine_args"`
	Spec       map[string]any `yaml:"spec"`
	Scaling    map[string]any `yaml:"scaling"`
	Timeouts   Timeouts       `yaml:"timeouts"`
}

// Timeouts bound how long a request may wait and run. Zero means "use the
// engine's baseline".
type Timeouts struct {
	ColdStart Duration `yaml:"cold_start"`
	Idle      Duration `yaml:"idle"`
	Request   Duration `yaml:"request"`
}

// Model is a name clients can request, served by one unit.
type Model struct {
	Unit    string   `yaml:"unit"`
	Aliases []string `yaml:"aliases"`
}

// Duration is a time.Duration written as "90s", "5m" and so on.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (use forms like 90s or 5m)", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// baselines are the per-engine timeouts used when a unit sets none. They come
// from the measured cold starts in docs/spike-findings.md.
var baselines = map[string]Timeouts{
	"llama.cpp": {ColdStart: Duration(5 * time.Minute), Idle: Duration(60 * time.Second), Request: Duration(5 * time.Minute)},
	"strata":    {ColdStart: Duration(20 * time.Minute), Idle: Duration(15 * time.Minute), Request: Duration(5 * time.Minute)},
}

// Engines lists the engines a unit may use.
func Engines() []string {
	out := make([]string, 0, len(baselines))
	for k := range baselines {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var envRef = regexp.MustCompile(`\$\{env\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw, os.Getenv)
}

// Parse validates configuration text. ${env.NAME} is replaced with the value
// of that environment variable, so secrets stay out of the file.
func Parse(raw []byte, getenv func(string) string) (*Config, error) {
	text := envRef.ReplaceAllStringFunc(string(raw), func(m string) string {
		return getenv(envRef.FindStringSubmatch(m)[1])
	})
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true) // a misspelt key is an error, not a silently ignored setting
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Gateway.Listen == "" {
		c.Gateway.Listen = ":8080"
	}
	keys := c.Gateway.APIKeys[:0]
	for _, k := range c.Gateway.APIKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	c.Gateway.APIKeys = keys
	if len(keys) == 0 {
		return fmt.Errorf("gateway.api_keys is empty: set at least one key (is the environment variable it refers to set?)")
	}
	if len(c.Units) == 0 {
		return fmt.Errorf("no units defined")
	}
	for name, u := range c.Units {
		base, ok := baselines[u.Engine]
		if !ok {
			return fmt.Errorf("units.%s.engine: %q is not one of %s", name, u.Engine, strings.Join(Engines(), ", "))
		}
		if u.Timeouts.ColdStart == 0 {
			u.Timeouts.ColdStart = base.ColdStart
		}
		if u.Timeouts.Idle == 0 {
			u.Timeouts.Idle = base.Idle
		}
		if u.Timeouts.Request == 0 {
			u.Timeouts.Request = base.Request
		}
		c.Units[name] = u
	}
	seen := map[string]string{}
	for name, m := range c.Models {
		if _, ok := c.Units[m.Unit]; !ok {
			return fmt.Errorf("models.%s.unit: no unit named %q", name, m.Unit)
		}
		for _, n := range append([]string{name}, m.Aliases...) {
			if other, dup := seen[n]; dup {
				return fmt.Errorf("model name %q is used by both %s and %s", n, other, name)
			}
			seen[n] = name
		}
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("no models defined")
	}
	return nil
}

// BaseURL returns where the unit's agent is reached, or "" if the unit has no
// endpoint yet.
func (u Unit) BaseURL() string {
	if u.URL != "" {
		return strings.TrimRight(u.URL, "/")
	}
	if u.EndpointID != "" {
		return "https://" + u.EndpointID + ".api.runpod.ai"
	}
	return ""
}
