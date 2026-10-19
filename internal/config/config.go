package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultListenAddress         = "0.0.0.0:8080"
	DefaultCooldown              = 120 * time.Second
	DefaultRecoveryWait          = 30 * time.Second
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultLogLevel              = "WARN"
)

// LogLevel is a configurable minimum severity threshold. Lines at or above
// this threshold are emitted. Valid values are INFO, WARN, and ERROR.
type LogLevel string

const (
	LogLevelInfo  LogLevel = "INFO"
	LogLevelWarn  LogLevel = "WARN"
	LogLevelError LogLevel = "ERROR"
)

func (l LogLevel) severity() int {
	switch l {
	case LogLevelError:
		return 30
	case LogLevelWarn:
		return 20
	default:
		return 10
	}
}

// Enabled reports whether a line at the given level passes this threshold.
func (l LogLevel) Enabled(level LogLevel) bool {
	return level.severity() >= l.severity()
}

// ReasoningEffort is the provider reasoning intensity. Nil means null/disabled.
type ReasoningEffort string

const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
	ReasoningEffortXHigh  ReasoningEffort = "xhigh"
	ReasoningEffortMax    ReasoningEffort = "max"
)

const reasoningEffortErrorMsg = "reasoning_effort must be one of none, low, medium, high, xhigh, max, null"

// ReasoningEffortErrorMessage is the shared enum message, also used for
// client-supplied values rejected by the proxy.
const ReasoningEffortErrorMessage = reasoningEffortErrorMsg

// IsValid reports whether the effort is one of the allowed non-null values.
// Comparison is case-insensitive and trims surrounding whitespace so that
// programmatic Config values like "MAX" or " Max " are accepted.
func (r ReasoningEffort) IsValid() bool {
	switch r.Normalize() {
	case ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium,
		ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax:
		return true
	default:
		return false
	}
}

// Normalize lowercases and trims the effort so programmatic Config values
// like "MAX" behave like loaded YAML values.
func (r ReasoningEffort) Normalize() ReasoningEffort {
	return ReasoningEffort(strings.ToLower(strings.TrimSpace(string(r))))
}

// ParseReasoningEffort normalizes and validates a reasoning_effort string
// value. It never accepts null; callers handle null separately.
func ParseReasoningEffort(s string) (*ReasoningEffort, error) {
	return parseReasoningEffort(s)
}

// parseReasoningEffort normalizes and validates a reasoning_effort string value.
func parseReasoningEffort(s string) (*ReasoningEffort, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf(reasoningEffortErrorMsg)
	}
	lowered := ReasoningEffort(trimmed).Normalize()
	if !lowered.IsValid() {
		return nil, fmt.Errorf(reasoningEffortErrorMsg)
	}
	v := lowered
	return &v, nil
}

// providerReasoningEffortError scopes the shared enum message to one provider.
func providerReasoningEffortError(index int) error {
	return fmt.Errorf("providers[%d].%s", index, reasoningEffortErrorMsg)
}

func modelReasoningEffortError(name string) error {
	return fmt.Errorf("models[%q].%s", name, reasoningEffortErrorMsg)
}

// Config is the validated runtime configuration for the proxy.
type Config struct {
	ListenAddress         string
	Cooldown              time.Duration
	RecoveryWait          time.Duration
	ResponseHeaderTimeout time.Duration
	LogLevel              LogLevel
	ReasoningEffort       *ReasoningEffort
	FailoverOn404         bool

	// FailoverOnAuthError treats upstream 401 and 403 responses as
	// Failover Failures that cool the whole Provider (its key is rejected).
	// Off by default so existing configurations keep passing them through.
	FailoverOnAuthError bool

	// MaxRoutingTime bounds how long one request may spend finding a
	// Provider (failovers and Recovery Waits included). Zero means no limit.
	MaxRoutingTime time.Duration

	// AllowClientReasoningEffort lets a valid client-supplied reasoning_effort
	// win over Virtual Model defaults and the global value (but never over a
	// Provider override). A Virtual Model may override this switch.
	AllowClientReasoningEffort bool

	// Models holds optional per-Virtual-Model settings, keyed by Virtual Model.
	Models map[string]ModelSettings

	Providers []Provider
}

// ModelSettings are optional per-Virtual-Model settings.
type ModelSettings struct {
	// ReasoningEffort is the Virtual Model's default effort; StripReasoningEffort
	// set means an explicit null (strip the field). Both zero inherits global.
	ReasoningEffort      *ReasoningEffort
	StripReasoningEffort bool
	// AllowClientReasoningEffort overrides the top-level switch when non-nil.
	AllowClientReasoningEffort *bool
}

// Provider is one OpenAI-compatible inference endpoint in the routing pool.
// A provider-level reasoning_effort overrides the global value; an explicit
// null strips the field for this provider. Leave both zero to inherit global.
//
// A Provider sets exactly one of ModelAlias (legacy: serves every Virtual
// Model and rewrites it to ModelAlias) or Models (serves only the listed
// Virtual Models, each rewritten to its own upstream model).
type Provider struct {
	Name       string
	BaseURL    string
	APIKey     string
	ModelAlias string
	Models     map[string]string
	Priority   int
	// Disabled providers stay configured (and reported) but are never routed to.
	Disabled bool

	ReasoningEffort      *ReasoningEffort
	StripReasoningEffort bool
}

// Legacy reports whether the Provider uses the single model_alias form.
func (p Provider) Legacy() bool {
	return len(p.Models) == 0
}

// UpstreamModel returns the upstream model this Provider uses for a Virtual
// Model, and whether it serves that Virtual Model at all.
func (p Provider) UpstreamModel(virtual string) (string, bool) {
	if p.Legacy() {
		return p.ModelAlias, true
	}
	upstream, ok := p.Models[virtual]
	return upstream, ok
}

type rawConfig struct {
	Server                     rawServer           `yaml:"server"`
	Cooldown                   string              `yaml:"cooldown"`
	RecoveryWait               string              `yaml:"recovery_wait"`
	ResponseHeaderTimeout      string              `yaml:"response_header_timeout"`
	MaxRoutingTime             string              `yaml:"max_routing_time"`
	LogLevel                   string              `yaml:"log_level"`
	ReasoningEffort            *string             `yaml:"reasoning_effort"` // present for KnownFields; value sourced from rawMap to distinguish null vs absent
	AllowClientReasoningEffort bool                `yaml:"allow_client_reasoning_effort"`
	FailoverOn404              bool                `yaml:"failover_on_404"`
	FailoverOnAuthError        bool                `yaml:"failover_on_auth_error"`
	Models                     map[string]rawModel `yaml:"models"`
	Providers                  []rawProvider       `yaml:"providers"`
}

type rawServer struct {
	ListenAddress string `yaml:"listen_address"`
}

type rawModel struct {
	ReasoningEffort            yaml.Node `yaml:"reasoning_effort"`
	AllowClientReasoningEffort *bool     `yaml:"allow_client_reasoning_effort"`
}

type rawProvider struct {
	Name       string            `yaml:"name"`
	BaseURL    string            `yaml:"base_url"`
	APIKey     string            `yaml:"api_key"`
	ModelAlias string            `yaml:"model_alias"`
	Models     map[string]string `yaml:"models"`
	Priority   *int              `yaml:"priority"`
	Enabled    *bool             `yaml:"enabled"`
	// ReasoningEffort keeps the raw node to distinguish absent (inherit
	// global, zero Node) from explicit null (strip for this provider).
	ReasoningEffort yaml.Node `yaml:"reasoning_effort"`
}

// Format is the encoding of a configuration document.
type Format int

const (
	FormatYAML Format = iota
	FormatJSON
)

// Load reads, defaults, normalizes, and validates one YAML configuration file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(data, FormatYAML, path)
}

// Parse decodes, defaults, normalizes, and validates a configuration
// document. source names the document in error messages. JSON documents use
// the same keys and rules as YAML files.
func Parse(data []byte, format Format, source string) (Config, error) {
	if format == FormatJSON {
		converted, err := jsonToYAML(data)
		if err != nil {
			return Config{}, fmt.Errorf("decode config %q: %w", source, err)
		}
		data = converted
	}

	// Inspect reasoning_effort presence and value before strict struct decoding,
	// to distinguish absent vs explicit null (both decode as nil into *string).
	var rawMap map[string]yaml.Node
	if err := yaml.Unmarshal(data, &rawMap); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", source, err)
	}
	node, ok := rawMap["reasoning_effort"]
	if !ok {
		return Config{}, fmt.Errorf("reasoning_effort is required")
	}
	var parsedReasoningEffort *ReasoningEffort
	if node.Tag == "!!null" {
		parsedReasoningEffort = nil
	} else {
		if node.Tag != "!!str" {
			return Config{}, fmt.Errorf(reasoningEffortErrorMsg)
		}
		parsed, err := parseReasoningEffort(node.Value)
		if err != nil {
			return Config{}, err
		}
		parsedReasoningEffort = parsed
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", source, err)
	}
	var extraDocument any
	if err := decoder.Decode(&extraDocument); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("decode config %q: multiple YAML documents are not supported", source)
		}
		return Config{}, fmt.Errorf("decode config %q: %w", source, err)
	}

	cfg := Config{
		ListenAddress:              strings.TrimSpace(raw.Server.ListenAddress),
		LogLevel:                   LogLevel(DefaultLogLevel),
		ReasoningEffort:            parsedReasoningEffort,
		FailoverOn404:              raw.FailoverOn404,
		FailoverOnAuthError:        raw.FailoverOnAuthError,
		AllowClientReasoningEffort: raw.AllowClientReasoningEffort,
		Providers:                  make([]Provider, 0, len(raw.Providers)),
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = DefaultListenAddress
	}

	var err error
	if cfg.Cooldown, err = parseDuration("cooldown", raw.Cooldown, DefaultCooldown); err != nil {
		return Config{}, err
	}
	if cfg.RecoveryWait, err = parseDuration("recovery_wait", raw.RecoveryWait, DefaultRecoveryWait); err != nil {
		return Config{}, err
	}
	if cfg.ResponseHeaderTimeout, err = parseDuration("response_header_timeout", raw.ResponseHeaderTimeout, DefaultResponseHeaderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxRoutingTime, err = parseOptionalDuration("max_routing_time", raw.MaxRoutingTime); err != nil {
		return Config{}, err
	}
	if cfg.LogLevel, err = parseLogLevel(raw.LogLevel); err != nil {
		return Config{}, err
	}

	if len(raw.Models) > 0 {
		cfg.Models = make(map[string]ModelSettings, len(raw.Models))
		for rawName, rawSettings := range raw.Models {
			name := strings.TrimSpace(rawName)
			if name == "" {
				return Config{}, fmt.Errorf("models must not contain an empty Virtual Model name")
			}
			if _, exists := cfg.Models[name]; exists {
				return Config{}, fmt.Errorf("models[%q] is declared more than once", name)
			}
			settings, err := normalizeModel(name, rawSettings)
			if err != nil {
				return Config{}, err
			}
			cfg.Models[name] = settings
		}
	}

	for index, rawProvider := range raw.Providers {
		provider, err := normalizeProvider(index, rawProvider)
		if err != nil {
			return Config{}, err
		}
		cfg.Providers = append(cfg.Providers, provider)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// jsonToYAML re-encodes a JSON object as YAML so both formats share one
// strict decoding path (unknown keys, null handling, typed fields).
func jsonToYAML(data []byte) ([]byte, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("invalid JSON: unexpected data after the top-level value")
	}
	if _, ok := document.(map[string]any); !ok {
		return nil, fmt.Errorf("invalid JSON: the document must be an object")
	}
	return yaml.Marshal(document)
}

// Validate ensures a Config can be used to start the proxy. An empty
// Provider list is valid: the proxy then answers 503 until one is configured.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return fmt.Errorf("server.listen_address must not be empty")
	}
	if c.Cooldown <= 0 {
		return fmt.Errorf("cooldown must be greater than zero")
	}
	if c.RecoveryWait <= 0 {
		return fmt.Errorf("recovery_wait must be greater than zero")
	}
	if c.ResponseHeaderTimeout <= 0 {
		return fmt.Errorf("response_header_timeout must be greater than zero")
	}
	if c.MaxRoutingTime < 0 {
		return fmt.Errorf("max_routing_time must not be negative")
	}
	if c.ReasoningEffort != nil && !c.ReasoningEffort.IsValid() {
		return fmt.Errorf(reasoningEffortErrorMsg)
	}
	for name, settings := range c.Models {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("models[%q] must be a non-empty name without surrounding whitespace", name)
		}
		if settings.ReasoningEffort != nil && !settings.ReasoningEffort.IsValid() {
			return modelReasoningEffortError(name)
		}
		if settings.StripReasoningEffort && settings.ReasoningEffort != nil {
			return fmt.Errorf("models[%q].reasoning_effort must be either null or an effort value", name)
		}
	}

	seenNames := make(map[string]int, len(c.Providers))
	seen := make(map[providerIdentity]struct{}, len(c.Providers))
	for index, provider := range c.Providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			return fmt.Errorf("providers[%d].name must not be empty", index)
		}
		if previousIndex, exists := seenNames[name]; exists {
			return fmt.Errorf("providers[%d].name duplicates providers[%d].name", index, previousIndex)
		}
		seenNames[name] = index
		if strings.TrimSpace(provider.BaseURL) == "" {
			return fmt.Errorf("providers[%d].base_url must not be empty", index)
		}
		if strings.TrimSpace(provider.APIKey) == "" {
			return fmt.Errorf("providers[%d].api_key must not be empty", index)
		}
		hasAlias := strings.TrimSpace(provider.ModelAlias) != ""
		hasModels := provider.Models != nil
		switch {
		case hasAlias && hasModels:
			return fmt.Errorf("providers[%d] must set exactly one of model_alias or models, not both", index)
		case !hasAlias && !hasModels:
			return fmt.Errorf("providers[%d].model_alias must not be empty (or set models)", index)
		case hasModels && len(provider.Models) == 0:
			return fmt.Errorf("providers[%d].models must not be empty", index)
		}
		for virtual, upstream := range provider.Models {
			if strings.TrimSpace(virtual) == "" || strings.TrimSpace(virtual) != virtual {
				return fmt.Errorf("providers[%d].models must use non-empty Virtual Model names without surrounding whitespace", index)
			}
			if strings.TrimSpace(upstream) == "" {
				return fmt.Errorf("providers[%d].models[%q] must name an upstream model", index, virtual)
			}
		}
		if provider.ReasoningEffort != nil && !provider.ReasoningEffort.IsValid() {
			return providerReasoningEffortError(index)
		}
		if provider.StripReasoningEffort && provider.ReasoningEffort != nil {
			return fmt.Errorf("providers[%d].reasoning_effort must be either null or an effort value", index)
		}
		if _, err := parseProviderBaseURL(index, provider.BaseURL); err != nil {
			return err
		}
		definitionKey := providerIdentity{
			baseURL:    provider.BaseURL,
			apiKey:     provider.APIKey,
			modelAlias: provider.ModelAlias,
			models:     modelsFingerprint(provider.Models),
			priority:   provider.Priority,
		}
		if _, exists := seen[definitionKey]; exists {
			return fmt.Errorf("providers[%d] duplicates another Provider definition", index)
		}
		seen[definitionKey] = struct{}{}
	}
	return nil
}

type providerIdentity struct {
	baseURL    string
	apiKey     string
	modelAlias string
	models     string
	priority   int
}

func modelsFingerprint(models map[string]string) string {
	if len(models) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(models) // map keys are sorted
	return string(encoded)
}

func parseDuration(field, value string, defaultValue time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration such as 60s: %w", field, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", field)
	}
	return duration, nil
}

// parseOptionalDuration parses a duration where empty or zero means "off".
func parseOptionalDuration(field, value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration such as 60s: %w", field, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s must not be negative", field)
	}
	return duration, nil
}

func parseLogLevel(value string) (LogLevel, error) {
	switch LogLevel(strings.ToUpper(strings.TrimSpace(value))) {
	case "":
		return LogLevel(DefaultLogLevel), nil
	case LogLevelInfo:
		return LogLevelInfo, nil
	case LogLevelWarn:
		return LogLevelWarn, nil
	case LogLevelError:
		return LogLevelError, nil
	default:
		return "", fmt.Errorf("log_level must be one of INFO, WARN, or ERROR")
	}
}

// parseEffortNode reads an optional reasoning_effort node: zero node means
// absent (inherit), null means strip, a string must be in the enum.
func parseEffortNode(node yaml.Node, invalid error) (*ReasoningEffort, bool, error) {
	if node.IsZero() {
		return nil, false, nil
	}
	switch node.Tag {
	case "!!null":
		return nil, true, nil
	case "!!str":
		value, err := parseReasoningEffort(node.Value)
		if err != nil {
			return nil, false, invalid
		}
		return value, false, nil
	default:
		return nil, false, invalid
	}
}

func normalizeModel(name string, raw rawModel) (ModelSettings, error) {
	effort, strip, err := parseEffortNode(raw.ReasoningEffort, modelReasoningEffortError(name))
	if err != nil {
		return ModelSettings{}, err
	}
	return ModelSettings{
		ReasoningEffort:            effort,
		StripReasoningEffort:       strip,
		AllowClientReasoningEffort: raw.AllowClientReasoningEffort,
	}, nil
}

func normalizeProvider(index int, raw rawProvider) (Provider, error) {
	if raw.Priority == nil {
		return Provider{}, fmt.Errorf("providers[%d].priority is required", index)
	}
	parsed, err := parseProviderBaseURL(index, raw.BaseURL)
	if err != nil {
		return Provider{}, err
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	normalizedURL := strings.TrimRight(parsed.String(), "/")

	effortOverride, stripEffort, err := parseEffortNode(raw.ReasoningEffort, providerReasoningEffortError(index))
	if err != nil {
		return Provider{}, err
	}

	var models map[string]string
	if raw.Models != nil {
		models = make(map[string]string, len(raw.Models))
		for virtual, upstream := range raw.Models {
			name := strings.TrimSpace(virtual)
			if _, exists := models[name]; exists {
				return Provider{}, fmt.Errorf("providers[%d].models[%q] is declared more than once", index, name)
			}
			models[name] = strings.TrimSpace(upstream)
		}
	}

	provider := Provider{
		Name:       strings.TrimSpace(raw.Name),
		BaseURL:    normalizedURL,
		APIKey:     strings.TrimSpace(raw.APIKey),
		ModelAlias: strings.TrimSpace(raw.ModelAlias),
		Models:     models,
		Priority:   *raw.Priority,
		Disabled:   raw.Enabled != nil && !*raw.Enabled,

		ReasoningEffort:      effortOverride,
		StripReasoningEffort: stripEffort,
	}
	return provider, nil
}

func parseProviderBaseURL(index int, value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("providers[%d].base_url must be an absolute HTTP(S) URL", index)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("providers[%d].base_url must use http or https", index)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("providers[%d].base_url must not contain credentials, query parameters, or fragments", index)
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/chat/completions") {
		return nil, fmt.Errorf("providers[%d].base_url must be an API root, not a chat-completions URL", index)
	}
	return parsed, nil
}
