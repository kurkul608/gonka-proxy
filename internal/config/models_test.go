package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

const multiModelYAML = `reasoning_effort: high
allow_client_reasoning_effort: true
max_routing_time: 45s
failover_on_auth_error: true
models:
  chat-fast:
    reasoning_effort: low
    allow_client_reasoning_effort: false
  chat-smart: {}
  chat-plain:
    reasoning_effort: ~
providers:
  - name: gate-a
    base_url: https://a.example/v1
    api_key: key-a-0123456789
    priority: 100
    models:
      chat-fast: deepseek-v4-flash
      chat-smart: deepseek-v4
  - name: gate-b
    base_url: https://b.example/v1/
    api_key: key-b-0123456789
    priority: 50
    enabled: false
    reasoning_effort: ~
    models:
      chat-fast: " flash-b "
  - name: legacy
    base_url: https://legacy.example/v1
    api_key: key-legacy-0123
    model_alias: legacy-model
    priority: 10
`

func TestParseMultiModelConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(multiModelYAML), config.FormatYAML, "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.AllowClientReasoningEffort || !cfg.FailoverOnAuthError {
		t.Errorf("AllowClientReasoningEffort=%t FailoverOnAuthError=%t, want both true", cfg.AllowClientReasoningEffort, cfg.FailoverOnAuthError)
	}
	if cfg.MaxRoutingTime != 45*time.Second {
		t.Errorf("MaxRoutingTime = %s, want 45s", cfg.MaxRoutingTime)
	}
	if got := cfg.Models["chat-fast"]; got.ReasoningEffort == nil || *got.ReasoningEffort != config.ReasoningEffortLow ||
		got.AllowClientReasoningEffort == nil || *got.AllowClientReasoningEffort {
		t.Errorf("chat-fast settings = %+v, want low effort and client effort disallowed", got)
	}
	if got, ok := cfg.Models["chat-smart"]; !ok || got.ReasoningEffort != nil || got.StripReasoningEffort || got.AllowClientReasoningEffort != nil {
		t.Errorf("chat-smart settings = %+v (present=%t), want empty settings", got, ok)
	}
	if got := cfg.Models["chat-plain"]; !got.StripReasoningEffort {
		t.Errorf("chat-plain settings = %+v, want strip", got)
	}

	a, b, legacy := cfg.Providers[0], cfg.Providers[1], cfg.Providers[2]
	if a.Legacy() || !legacy.Legacy() {
		t.Errorf("Legacy() = %t/%t, want false/true", a.Legacy(), legacy.Legacy())
	}
	if upstream, ok := a.UpstreamModel("chat-smart"); !ok || upstream != "deepseek-v4" {
		t.Errorf("gate-a chat-smart = %q, %t", upstream, ok)
	}
	if _, ok := a.UpstreamModel("unknown"); ok {
		t.Error("gate-a serves an unlisted Virtual Model")
	}
	if upstream, ok := legacy.UpstreamModel("anything"); !ok || upstream != "legacy-model" {
		t.Errorf("legacy anything = %q, %t, want legacy-model", upstream, ok)
	}
	if !b.Disabled || a.Disabled {
		t.Errorf("Disabled = %t/%t, want gate-b only", a.Disabled, b.Disabled)
	}
	if b.Models["chat-fast"] != "flash-b" {
		t.Errorf("gate-b upstream = %q, want trimmed flash-b", b.Models["chat-fast"])
	}
	if b.BaseURL != "https://b.example/v1" {
		t.Errorf("gate-b base URL = %q, want normalized", b.BaseURL)
	}
	if !b.StripReasoningEffort {
		t.Error("gate-b reasoning_effort ~ should strip")
	}
}

func TestParseRejectsInvalidMultiModelConfig(t *testing.T) {
	provider := func(extra string) string {
		return "reasoning_effort: max\nproviders:\n  - name: p\n    base_url: https://p.example/v1\n    api_key: key\n    priority: 1\n" + extra
	}
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{"both model_alias and models", provider("    model_alias: m\n    models:\n      v: m\n"), "exactly one of model_alias or models"},
		{"neither model_alias nor models", provider(""), "providers[0].model_alias must not be empty (or set models)"},
		{"empty models map", provider("    models: {}\n"), "providers[0].models must not be empty"},
		{"empty upstream model", provider("    models:\n      v: \"\"\n"), `providers[0].models["v"] must name an upstream model`},
		{"empty virtual model", provider("    models:\n      \" \": m\n"), "non-empty Virtual Model names"},
		{"unknown provider field", provider("    model_alias: m\n    weight: 3\n"), "field weight not found"},
		{"unknown top-level field", "reasoning_effort: max\ngates: []\n", "field gates not found"},
		{"unknown model setting", "reasoning_effort: max\nmodels:\n  v:\n    temperature: 1\n", "field temperature not found"},
		{"invalid model effort", "reasoning_effort: max\nmodels:\n  v:\n    reasoning_effort: extreme\n", `models["v"].reasoning_effort must be one of`},
		{"quoted null model effort", "reasoning_effort: max\nmodels:\n  v:\n    reasoning_effort: \"null\"\n", `models["v"].reasoning_effort must be one of`},
		{"non-bool enabled", provider("    model_alias: m\n    enabled: maybe\n"), "decode config"},
		{"negative max routing time", "reasoning_effort: max\nmax_routing_time: -5s\n", "max_routing_time must not be negative"},
		{"invalid max routing time", "reasoning_effort: max\nmax_routing_time: soon\n", "max_routing_time must be a valid duration"},
		{"missing global effort", "providers: []\n", "reasoning_effort is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Parse([]byte(test.contents), config.FormatYAML, "test")
			if err == nil {
				t.Fatal("Parse succeeded for invalid configuration")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseMaxRoutingTime(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"0s", 0},
		{"1500ms", 1500 * time.Millisecond},
		{"2m", 2 * time.Minute},
	} {
		t.Run(test.value, func(t *testing.T) {
			contents := "reasoning_effort: max\n"
			if test.value != "" {
				contents += "max_routing_time: " + test.value + "\n"
			}
			cfg, err := config.Parse([]byte(contents), config.FormatYAML, "test")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.MaxRoutingTime != test.want {
				t.Fatalf("MaxRoutingTime = %s, want %s", cfg.MaxRoutingTime, test.want)
			}
		})
	}
}

func TestParseAcceptsZeroProviders(t *testing.T) {
	for name, contents := range map[string]string{
		"absent": "reasoning_effort: max\n",
		"empty":  "reasoning_effort: max\nproviders: []\n",
		"null":   "reasoning_effort: max\nproviders:\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := config.Parse([]byte(contents), config.FormatYAML, "test")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(cfg.Providers) != 0 {
				t.Fatalf("providers = %d, want 0", len(cfg.Providers))
			}
		})
	}
}

func TestParseJSONMatchesYAML(t *testing.T) {
	yamlConfig, err := config.Parse([]byte(multiModelYAML), config.FormatYAML, "yaml")
	if err != nil {
		t.Fatalf("Parse YAML: %v", err)
	}
	jsonDocument, err := json.Marshal(yamlConfig.Document())
	if err != nil {
		t.Fatalf("encode document: %v", err)
	}
	jsonConfig, err := config.Parse(jsonDocument, config.FormatJSON, "json")
	if err != nil {
		t.Fatalf("Parse JSON %s: %v", jsonDocument, err)
	}
	if yamlConfig.Version() != jsonConfig.Version() {
		t.Fatalf("versions differ: yaml %s json %s", yamlConfig.Version(), jsonConfig.Version())
	}
}

func TestParseJSONRules(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{"not an object", `[1,2]`, "must be an object"},
		{"malformed", `{"reasoning_effort":`, "invalid JSON"},
		{"trailing data", `{"reasoning_effort":"max"} {}`, "unexpected data"},
		{"unknown field", `{"reasoning_effort":"max","provider":[]}`, "field provider not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Parse([]byte(test.contents), config.FormatJSON, "json")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}

	cfg, err := config.Parse([]byte(`{"reasoning_effort":null,"models":{"v":{"reasoning_effort":null}},"providers":[{"name":"p","base_url":"https://p.example","api_key":"k","models":{"v":"m"},"priority":3,"reasoning_effort":"HIGH"}]}`), config.FormatJSON, "json")
	if err != nil {
		t.Fatalf("Parse JSON: %v", err)
	}
	if cfg.ReasoningEffort != nil || !cfg.Models["v"].StripReasoningEffort {
		t.Errorf("JSON null efforts not parsed as null: %+v", cfg)
	}
	if cfg.Providers[0].ReasoningEffort == nil || *cfg.Providers[0].ReasoningEffort != config.ReasoningEffortHigh {
		t.Errorf("provider effort = %v, want high", cfg.Providers[0].ReasoningEffort)
	}
}

func TestVersionIsStable(t *testing.T) {
	base, err := config.Parse([]byte(multiModelYAML), config.FormatYAML, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Same content, different key order, quoting, defaults spelled out.
	reordered := strings.Replace(multiModelYAML, "reasoning_effort: high\n", "", 1) +
		"reasoning_effort: \"HIGH\"\ncooldown: 2m\nrecovery_wait: 30s\n"
	same, err := config.Parse([]byte(reordered), config.FormatYAML, "b")
	if err != nil {
		t.Fatal(err)
	}
	if base.Version() != same.Version() {
		t.Fatalf("equivalent configs have different versions")
	}
	if len(base.Version()) != 64 {
		t.Fatalf("version %q is not a hex sha256", base.Version())
	}

	rotated, err := config.Parse([]byte(strings.Replace(multiModelYAML, "key-a-0123456789", "key-a-rotated-99", 1)), config.FormatYAML, "c")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Version() == base.Version() {
		t.Fatal("rotating an API key must change the version")
	}
}

func TestDocumentRedactsAPIKeys(t *testing.T) {
	cfg, err := config.Parse([]byte(multiModelYAML), config.FormatYAML, "test")
	if err != nil {
		t.Fatal(err)
	}
	redacted := cfg.Document().Redacted()
	encoded, _ := json.Marshal(redacted)
	for _, secret := range []string{"key-a-0123456789", "key-b-0123456789", "key-legacy-0123"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("redacted document leaks %q: %s", secret, encoded)
		}
	}
	if got := redacted.Providers[0].APIKey; got != "****6789" {
		t.Errorf("redacted key = %q, want ****6789", got)
	}
	if got := config.RedactSecret("short"); got != "****" {
		t.Errorf("short secret redacted to %q, want ****", got)
	}
	if cfg.Document().Providers[0].APIKey != "key-a-0123456789" {
		t.Error("Redacted must not modify the original document")
	}
}

func TestMarshalFileRoundTrips(t *testing.T) {
	cfg, err := config.Parse([]byte(multiModelYAML), config.FormatYAML, "test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := cfg.MarshalFile()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.WriteFileAtomic(path, data); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load persisted config: %v\n%s", err, data)
	}
	if reloaded.Version() != cfg.Version() {
		t.Fatalf("persisted config version changed\n%s", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
