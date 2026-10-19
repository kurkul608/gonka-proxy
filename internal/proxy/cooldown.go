package proxy

import (
	"net/http"
	"sync"
	"time"
)

// cooldownScope says how much of a Provider a Failover Failure benches.
type cooldownScope int

const (
	// scopeModel benches one (Provider, upstream model) pair: the failure is
	// about that model on that Provider (5xx, timeouts, 404, unsupported
	// reasoning_effort, broken streams).
	scopeModel cooldownScope = iota
	// scopeProvider benches the whole Provider: the failure is about its key,
	// account or reachability (401/403 when enabled, 402, 429, network errors).
	scopeProvider
)

func (s cooldownScope) String() string {
	if s == scopeProvider {
		return "provider"
	}
	return "model"
}

// failureKind classifies a Failover Failure for status reporting.
const (
	failureKindStatus  = "status"
	failureKindTimeout = "timeout"
	failureKindNetwork = "network"
	failureKindStream  = "stream"
)

// failureRecord is the last Failover Failure seen for a cooldown key.
type failureRecord struct {
	kind     string
	category string
	status   int
	scope    cooldownScope
	at       time.Time
}

// scopeFor decides whether a failure cools one model or the whole Provider.
// See ADR 0004.
func scopeFor(kind string, statusCode int) cooldownScope {
	switch kind {
	case failureKindNetwork:
		return scopeProvider
	case failureKindStatus:
		switch statusCode {
		case http.StatusUnauthorized, http.StatusForbidden,
			http.StatusPaymentRequired, http.StatusTooManyRequests:
			return scopeProvider
		}
	}
	return scopeModel
}

// providerID identifies a Provider across configuration reloads. Cooldown
// state survives a reload only while name, base URL and API key are unchanged.
type providerID struct {
	name    string
	baseURL string
	apiKey  string
}

// cooldownKey addresses one cooldown: a whole Provider (model == "") or one
// upstream model on it.
type cooldownKey struct {
	provider providerID
	model    string
}

type cooldownEntry struct {
	until   time.Time
	version uint64
	last    *failureRecord
}

// cooldownRegistry is shared by every configuration snapshot so cooldowns
// survive hot reloads.
type cooldownRegistry struct {
	mu      sync.Mutex
	seq     uint64
	entries map[cooldownKey]*cooldownEntry
}

func newCooldownRegistry() *cooldownRegistry {
	return &cooldownRegistry{entries: make(map[cooldownKey]*cooldownEntry)}
}

// available reports whether neither the Provider nor its upstream model is
// cooling down.
func (c *cooldownRegistry) available(id providerID, model string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[cooldownKey{provider: id}]; entry != nil && now.Before(entry.until) {
		return false
	}
	if entry := c.entries[cooldownKey{provider: id, model: model}]; entry != nil && now.Before(entry.until) {
		return false
	}
	return true
}

// mark starts (or extends) a cooldown and records the failure behind it.
func (c *cooldownRegistry) mark(key cooldownKey, duration time.Duration, failure *failureRecord) {
	now := time.Now()
	until := now.Add(duration)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		entry = &cooldownEntry{}
		c.entries[key] = entry
	}
	c.seq++
	entry.version = c.seq
	if until.After(entry.until) {
		entry.until = until
	}
	if failure != nil {
		failure.at = now
		entry.last = failure
	}
}

// versions snapshots every entry's version before a Recovery Wait.
func (c *cooldownRegistry) versions() map[cooldownKey]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	versions := make(map[cooldownKey]uint64, len(c.entries))
	for key, entry := range c.entries {
		versions[key] = entry.version
	}
	return versions
}

// clear ends every cooldown that was not renewed since versions were taken
// and returns the cleared keys.
func (c *cooldownRegistry) clear(versions map[cooldownKey]uint64) []cooldownKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cleared []cooldownKey
	for key, entry := range c.entries {
		version, seen := versions[key]
		if !seen || entry.version != version || entry.until.IsZero() {
			continue
		}
		entry.until = time.Time{}
		cleared = append(cleared, key)
	}
	return cleared
}

// retain drops state for Providers or models that are no longer configured.
func (c *cooldownRegistry) retain(keep func(cooldownKey) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if !keep(key) {
			delete(c.entries, key)
		}
	}
}

// cooldownView is a point-in-time copy of one entry for status reporting.
type cooldownView struct {
	until time.Time
	last  *failureRecord
}

func (c *cooldownRegistry) view(key cooldownKey) cooldownView {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return cooldownView{}
	}
	view := cooldownView{until: entry.until}
	if entry.last != nil {
		last := *entry.last
		view.last = &last
	}
	return view
}
