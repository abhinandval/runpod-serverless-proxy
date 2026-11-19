package config

import (
	"strings"
	"testing"
	"time"
)

const sample = `
gateway:
  listen: ":9000"
  api_keys: ["${env.GATEWAY_KEY}"]
fleet:
  provider: runpod
  inference_key: ${env.RUNPOD_INFERENCE_KEY}
  hf_token: ${env.HF_TOKEN}
units:
  llama:
    engine: llama.cpp
    endpoint_id: abc123
    spec: {gpu: {pools: [AMPERE_24]}, disk_gb: 40}
    timeouts: {cold_start: 3m}
  strata:
    engine: strata
    url: http://localhost:8080/
models:
  qwen-27b: {unit: llama, aliases: [gpt-4o-mini]}
  qwen-flash: {unit: strata}
`

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseAppliesEnvAndBaselines(t *testing.T) {
	c, err := Parse([]byte(sample), env(map[string]string{"GATEWAY_KEY": "k1", "RUNPOD_INFERENCE_KEY": "rk"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Listen != ":9000" || len(c.Gateway.APIKeys) != 1 || c.Gateway.APIKeys[0] != "k1" || c.Fleet.InferenceKey != "rk" || c.Fleet.HFToken != "" {
		t.Errorf("gateway=%+v fleet=%+v", c.Gateway, c.Fleet)
	}
	l, s := c.Units["llama"], c.Units["strata"]
	if time.Duration(l.Timeouts.ColdStart) != 3*time.Minute || time.Duration(l.Timeouts.Idle) != time.Minute || time.Duration(l.Timeouts.Request) != 5*time.Minute {
		t.Errorf("llama timeouts = %+v (explicit cold_start, baseline for the rest)", l.Timeouts)
	}
	if time.Duration(s.Timeouts.ColdStart) != 20*time.Minute || time.Duration(s.Timeouts.Idle) != 15*time.Minute {
		t.Errorf("strata timeouts = %+v", s.Timeouts)
	}
	if l.BaseURL() != "https://abc123.api.runpod.ai" || s.BaseURL() != "http://localhost:8080" {
		t.Errorf("base urls: %q %q", l.BaseURL(), s.BaseURL())
	}
}

func TestParseRejectsMistakes(t *testing.T) {
	ok := map[string]string{"GATEWAY_KEY": "k1"}
	cases := map[string]struct {
		edit func(string) string
		env  map[string]string
		want string
	}{
		"unset key variable": {func(s string) string { return s }, map[string]string{}, "api_keys is empty"},
		"misspelt field":     {func(s string) string { return strings.Replace(s, "endpoint_id", "endpoint", 1) }, ok, "field endpoint not found"},
		"unknown engine":     {func(s string) string { return strings.Replace(s, "engine: strata", "engine: vllm", 1) }, ok, "not one of"},
		"missing unit":       {func(s string) string { return strings.Replace(s, "unit: strata", "unit: nope", 1) }, ok, `no unit named "nope"`},
		"duplicate name":     {func(s string) string { return strings.Replace(s, "aliases: [gpt-4o-mini]", "aliases: [qwen-flash]", 1) }, ok, "used by both"},
		"bad duration":       {func(s string) string { return strings.Replace(s, "cold_start: 3m", "cold_start: soon", 1) }, ok, "not a duration"},
	}
	for name, tc := range cases {
		if _, err := Parse([]byte(tc.edit(sample)), env(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}
