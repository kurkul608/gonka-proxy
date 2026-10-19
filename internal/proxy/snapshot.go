package proxy

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

// snapshot is one immutable routing configuration. A request loads the
// current snapshot once and uses it until it finishes, so a hot reload never
// changes the Providers or settings of a request already in flight.
type snapshot struct {
	cfg      config.Config
	version  string
	loadedAt time.Time

	providers     []*provider // descending priority, declaration order for ties
	enabledCount  int
	hasLegacy     bool
	virtualModels map[string]struct{} // every Virtual Model named by models: or a Provider's models map

	transport *http.Transport
	client    *http.Client

	logger           Logger
	logLevel         config.LogLevel
	cooldownDuration time.Duration
	recoveryWait     time.Duration
	maxRoutingTime   time.Duration
	failoverOn404    bool
	failoverOnAuth   bool
	globalEffort     *config.ReasoningEffort
	allowClient      bool
	models           map[string]config.ModelSettings

	secrets         []string // API keys, longest first, for log redaction
	streamTailLimit int
}

type provider struct {
	config.Provider
	id      providerID
	chatURL string
}

// route is one candidate attempt: a Provider and the upstream model it uses
// for the requested Virtual Model.
type route struct {
	provider *provider
	upstream string
}

// buildSnapshot validates cfg and prepares an immutable snapshot. previous
// may be nil; its transport is reused when the timeout is unchanged so a
// reload keeps pooled upstream connections.
func buildSnapshot(cfg config.Config, logger Logger, previous *snapshot) (*snapshot, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid proxy config: %w", err)
	}
	logLevel := cfg.LogLevel
	if logLevel == "" {
		logLevel = config.LogLevelWarn
	}

	st := &snapshot{
		cfg:              cfg,
		version:          cfg.Version(),
		loadedAt:         time.Now(),
		virtualModels:    make(map[string]struct{}),
		logger:           logger,
		logLevel:         logLevel,
		cooldownDuration: cfg.Cooldown,
		recoveryWait:     cfg.RecoveryWait,
		maxRoutingTime:   cfg.MaxRoutingTime,
		failoverOn404:    cfg.FailoverOn404,
		failoverOnAuth:   cfg.FailoverOnAuthError,
		// Normalize programmatic Config values (e.g. "MAX") to lower; Load already normalizes.
		globalEffort: normalizeReasoningEffort(cfg.ReasoningEffort),
		allowClient:  cfg.AllowClientReasoningEffort,
		models:       cfg.Models,
	}
	for name := range cfg.Models {
		st.virtualModels[name] = struct{}{}
	}

	seenSecrets := make(map[string]struct{}, len(cfg.Providers))
	maxSecretLength := 0
	for _, configured := range cfg.Providers {
		chatURL, err := chatCompletionsURL(configured.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Provider base URL: %w", err)
		}
		st.providers = append(st.providers, &provider{
			Provider: configured,
			id:       providerID{name: configured.Name, baseURL: configured.BaseURL, apiKey: configured.APIKey},
			chatURL:  chatURL,
		})
		if !configured.Disabled {
			st.enabledCount++
		}
		if configured.Legacy() {
			st.hasLegacy = true
		}
		for virtual := range configured.Models {
			st.virtualModels[virtual] = struct{}{}
		}
		if secret := configured.APIKey; secret != "" {
			if _, seen := seenSecrets[secret]; !seen {
				seenSecrets[secret] = struct{}{}
				st.secrets = append(st.secrets, secret)
			}
			if len(secret) > maxSecretLength {
				maxSecretLength = len(secret)
			}
		}
	}
	sort.SliceStable(st.providers, func(i, j int) bool {
		return st.providers[i].Priority > st.providers[j].Priority
	})
	sort.Slice(st.secrets, func(i, j int) bool {
		return len(st.secrets[i]) > len(st.secrets[j])
	})
	st.streamTailLimit = streamAbortBuf + maxSecretLength

	if previous != nil && previous.cfg.ResponseHeaderTimeout == cfg.ResponseHeaderTimeout {
		st.transport = previous.transport
		st.client = previous.client
	} else {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("http.DefaultTransport is not an *http.Transport")
		}
		transport = transport.Clone()
		transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
		st.transport = transport
		st.client = &http.Client{Transport: transport}
	}
	return st, nil
}

// routes returns the enabled Providers that serve a Virtual Model, in
// routing order.
func (st *snapshot) routes(virtual string) []route {
	routes := make([]route, 0, len(st.providers))
	for _, candidate := range st.providers {
		if candidate.Disabled {
			continue
		}
		upstream, ok := candidate.UpstreamModel(virtual)
		if !ok {
			continue
		}
		routes = append(routes, route{provider: candidate, upstream: upstream})
	}
	return routes
}

// knows reports whether any configured Provider (enabled or not) or the
// models section could serve the Virtual Model. Legacy Providers serve
// every Virtual Model.
func (st *snapshot) knows(virtual string) bool {
	if st.hasLegacy {
		return true
	}
	_, ok := st.virtualModels[virtual]
	return ok
}

// clientEffortAllowed applies a Virtual Model's override of the top-level
// allow_client_reasoning_effort switch.
func (st *snapshot) clientEffortAllowed(virtual string) bool {
	if settings, ok := st.models[virtual]; ok && settings.AllowClientReasoningEffort != nil {
		return *settings.AllowClientReasoningEffort
	}
	return st.allowClient
}

// resolveEffort picks the reasoning_effort for one attempt (ADR 0006):
// Provider override (value or strip) > allowed client value > Virtual Model
// default (value or strip) > global value. Nil strips the field.
func (st *snapshot) resolveEffort(selected *provider, virtual string, client *config.ReasoningEffort) *config.ReasoningEffort {
	if selected.StripReasoningEffort {
		return nil
	}
	if override := normalizeReasoningEffort(selected.ReasoningEffort); override != nil {
		return override
	}
	if client != nil {
		return client
	}
	if settings, ok := st.models[virtual]; ok {
		if settings.StripReasoningEffort {
			return nil
		}
		if value := normalizeReasoningEffort(settings.ReasoningEffort); value != nil {
			return value
		}
	}
	return st.globalEffort
}

// listedModels returns the ids served by GET /v1/models: every configured
// Virtual Model in name order or, when only legacy Providers exist, each
// Model Alias once in priority order.
func (st *snapshot) listedModels() []string {
	ids := make([]string, 0, len(st.virtualModels))
	for name := range st.virtualModels {
		ids = append(ids, name)
	}
	if len(ids) > 0 {
		sort.Strings(ids)
		return ids
	}
	seen := make(map[string]struct{}, len(st.providers))
	for _, candidate := range st.providers {
		if !candidate.Legacy() {
			continue
		}
		if _, dup := seen[candidate.ModelAlias]; dup {
			continue
		}
		seen[candidate.ModelAlias] = struct{}{}
		ids = append(ids, candidate.ModelAlias)
	}
	return ids
}

// servesKey reports whether a Provider in this snapshot still uses the
// cooldown key, so reloads keep only live cooldown state.
func (st *snapshot) servesKey(key cooldownKey) bool {
	for _, candidate := range st.providers {
		if candidate.id != key.provider {
			continue
		}
		if key.model == "" {
			return true
		}
		if candidate.Legacy() {
			if candidate.ModelAlias == key.model {
				return true
			}
			continue
		}
		for _, upstream := range candidate.Models {
			if upstream == key.model {
				return true
			}
		}
	}
	return false
}

// label names a cooldown key in logs: the Provider name, plus the upstream
// model for multi-model Providers.
func (st *snapshot) label(key cooldownKey) string {
	name := st.redactProviderSecrets(key.provider.name)
	if key.model == "" {
		return name
	}
	for _, candidate := range st.providers {
		if candidate.id == key.provider && candidate.Legacy() {
			return name
		}
	}
	return name + " [" + st.redactProviderSecrets(key.model) + "]"
}

// logAt writes a line only if the configured level permits it.
func (st *snapshot) logAt(level config.LogLevel, format string, args ...any) {
	if !st.logLevel.Enabled(level) {
		return
	}
	st.logger.Printf(format, args...)
}

func (st *snapshot) payloadLoggingEnabled() bool {
	return st.logLevel == config.LogLevelInfo
}

func (st *snapshot) redactProviderSecrets(message string) string {
	for _, secret := range st.secrets {
		message = strings.ReplaceAll(message, "Bearer "+secret, "Bearer [REDACTED]")
		message = strings.ReplaceAll(message, "bearer "+secret, "bearer [REDACTED]")
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
	}
	return message
}

func (st *snapshot) sanitizeLogMessage(message string) string {
	return sanitizeErrorMessage(st.redactProviderSecrets(message))
}
