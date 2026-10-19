package proxy

import (
	"sort"
	"time"
)

// Status is the routing state reported by GET /admin/status.
type Status struct {
	Version          string           `json:"version"`
	LoadedAt         string           `json:"loaded_at"`
	Providers        int              `json:"providers"`
	EnabledProviders int              `json:"enabled_providers"`
	ProviderStatuses []ProviderStatus `json:"provider_status"`
}

// ProviderStatus is one Provider's state. CooldownUntil and LastFailure here
// describe Provider-wide cooldowns (key or account failures).
type ProviderStatus struct {
	Name          string         `json:"name"`
	Priority      int            `json:"priority"`
	Enabled       bool           `json:"enabled"`
	Legacy        bool           `json:"legacy"`
	Available     bool           `json:"available"`
	CooldownUntil *string        `json:"cooldown_until"`
	LastFailure   *FailureStatus `json:"last_failure"`
	Models        []ModelStatus  `json:"models"`
}

// ModelStatus is the state of one Virtual Model on a Provider. A legacy
// Provider reports a single entry with Virtual "*". Available and
// CooldownUntil combine the Provider-wide and per-model cooldowns;
// LastFailure is the most recent of the two.
type ModelStatus struct {
	Virtual       string         `json:"virtual"`
	Upstream      string         `json:"upstream"`
	Available     bool           `json:"available"`
	CooldownUntil *string        `json:"cooldown_until"`
	LastFailure   *FailureStatus `json:"last_failure"`
}

// FailureStatus is the last Failover Failure recorded for a cooldown.
type FailureStatus struct {
	// Kind is "status" (an HTTP status), "timeout", "network" or "stream".
	Kind     string `json:"kind"`
	Category string `json:"category"`
	Status   int    `json:"status,omitempty"`
	// Scope is "provider" (the whole Provider cooled) or "model".
	Scope string `json:"scope"`
	At    string `json:"at"`
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func cooldownUntil(now time.Time, views ...cooldownView) *string {
	var latest time.Time
	for _, view := range views {
		if view.until.After(now) && view.until.After(latest) {
			latest = view.until
		}
	}
	if latest.IsZero() {
		return nil
	}
	formatted := formatTime(latest)
	return &formatted
}

func lastFailure(views ...cooldownView) *FailureStatus {
	var latest *failureRecord
	for _, view := range views {
		if view.last != nil && (latest == nil || view.last.at.After(latest.at)) {
			latest = view.last
		}
	}
	if latest == nil {
		return nil
	}
	return &FailureStatus{
		Kind:     latest.kind,
		Category: latest.category,
		Status:   latest.status,
		Scope:    latest.scope.String(),
		At:       formatTime(latest.at),
	}
}

// Status reports the current configuration and cooldown state.
func (s *Server) Status() Status {
	st := s.state.Load()
	now := time.Now()
	status := Status{
		Version:          st.version,
		LoadedAt:         formatTime(st.loadedAt),
		Providers:        len(st.providers),
		EnabledProviders: st.enabledCount,
		ProviderStatuses: make([]ProviderStatus, 0, len(st.providers)),
	}
	for _, configured := range st.providers {
		providerView := s.cooldowns.view(cooldownKey{provider: configured.id})
		entry := ProviderStatus{
			Name:          configured.Name,
			Priority:      configured.Priority,
			Enabled:       !configured.Disabled,
			Legacy:        configured.Legacy(),
			Available:     !configured.Disabled && !providerView.until.After(now),
			CooldownUntil: cooldownUntil(now, providerView),
			LastFailure:   lastFailure(providerView),
		}
		pairs := make([][2]string, 0, len(configured.Models)+1)
		if configured.Legacy() {
			pairs = append(pairs, [2]string{"*", configured.ModelAlias})
		} else {
			for virtual, upstream := range configured.Models {
				pairs = append(pairs, [2]string{virtual, upstream})
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
		}
		for _, pair := range pairs {
			modelView := s.cooldowns.view(cooldownKey{provider: configured.id, model: pair[1]})
			entry.Models = append(entry.Models, ModelStatus{
				Virtual:       pair[0],
				Upstream:      pair[1],
				Available:     entry.Available && !modelView.until.After(now),
				CooldownUntil: cooldownUntil(now, providerView, modelView),
				LastFailure:   lastFailure(providerView, modelView),
			})
		}
		status.ProviderStatuses = append(status.ProviderStatuses, entry)
	}
	return status
}
