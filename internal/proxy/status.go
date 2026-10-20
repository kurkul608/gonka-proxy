package proxy

import (
	"math"
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

	// StatsWindowSeconds is the sliding window of Stats[].Window and the
	// percentiles; StatsEWMAAlpha weighs the newest sample in the EWMAs.
	StatsWindowSeconds float64      `json:"stats_window_seconds"`
	StatsEWMAAlpha     float64      `json:"stats_ewma_alpha"`
	Stats              []RouteStats `json:"stats"`
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
	status.StatsWindowSeconds = s.stats.settings().Window.Seconds()
	status.StatsEWMAAlpha = s.stats.settings().EWMAAlpha
	status.Stats = routeStatsFor(st, now)
	return status
}

// RouteStats is the traffic of one (Provider, Upstream Model) pair: every
// upstream attempt the proxy made to that pair, failovers included.
type RouteStats struct {
	// Provider is the Provider name, the value of the X-Gonka-Provider header.
	Provider string `json:"provider"`
	// Upstream is the Upstream Model, the value of the X-Gonka-Model header.
	Upstream string `json:"upstream"`
	// VirtualModels are the Virtual Models this Provider routes to Upstream
	// ("*" for a model_alias Provider), sorted.
	VirtualModels []string `json:"virtual_models"`
	Enabled       bool     `json:"enabled"`
	// Since is when counting started for this pair: process start, or the
	// reload that added it.
	Since  string      `json:"since"`
	Total  StatsCounts `json:"total"`
	Window StatsCounts `json:"window"`
	TTFBMs StatsModes  `json:"ttfb_ms"`
	TPS    StatsModes  `json:"tokens_per_second"`
	// LastSuccessAt, LastFailureAt and LastErrorClass are null until the
	// first success or failure.
	LastSuccessAt  *string `json:"last_success_at"`
	LastFailureAt  *string `json:"last_failure_at"`
	LastErrorClass *string `json:"last_error_class"`
}

// StatsCounts are attempt counters. Requests = Successes + Failures +
// Canceled + ClientErrors + Deadline.
type StatsCounts struct {
	Requests  uint64 `json:"requests"`
	Successes uint64 `json:"successes"`
	// Failures are failures of the Provider; FailuresByClass splits them.
	Failures        uint64          `json:"failures"`
	FailuresByClass FailuresByClass `json:"failures_by_class"`
	// Canceled attempts ended because the client went away.
	Canceled uint64 `json:"canceled"`
	// ClientErrors are client-caused 4xx answers (400, 422, ...) passed
	// back unchanged.
	ClientErrors uint64 `json:"client_errors"`
	// Deadline attempts were cut by the routing deadline: a client-chosen
	// X-Gonka-Max-Routing-Time ran out, or the deadline won the race with
	// the Provider's answer.
	Deadline uint64 `json:"deadline"`
}

// FailuresByClass splits failures by class.
type FailuresByClass struct {
	Timeout    uint64 `json:"timeout"`
	Server     uint64 `json:"5xx"`
	Client     uint64 `json:"4xx"`
	Connection uint64 `json:"connection"`
}

// StatsModes splits a metric by response mode.
type StatsModes struct {
	Stream    StatsMetric `json:"stream"`
	NonStream StatsMetric `json:"non_stream"`
}

// StatsMetric summarizes one latency or throughput metric. EWMA covers every
// sample since Since; P50 and P95 cover the samples inside the window. Each is
// null without samples.
type StatsMetric struct {
	EWMA          *float64 `json:"ewma"`
	P50           *float64 `json:"p50"`
	P95           *float64 `json:"p95"`
	Samples       uint64   `json:"samples"`
	WindowSamples int      `json:"window_samples"`
	// WindowSamplesTruncated: the window held more samples than are kept, so
	// P50/P95 describe only the newest ones.
	WindowSamplesTruncated bool `json:"window_samples_truncated"`
}

func formatStatsTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func optionalStatsTime(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := formatStatsTime(value)
	return &formatted
}

func roundedPointer(value float64) *float64 {
	rounded := math.Round(value*1000) / 1000
	return &rounded
}

func (c statsCounters) counts() StatsCounts {
	return StatsCounts{
		Requests:  c.requests,
		Successes: c.successes,
		Failures:  c.failures,
		FailuresByClass: FailuresByClass{
			Timeout:    c.timeout,
			Server:     c.server,
			Client:     c.client,
			Connection: c.connection,
		},
		Canceled:     c.canceled,
		ClientErrors: c.clientErrors,
		Deadline:     c.deadline,
	}
}

func (m metricView) metric() StatsMetric {
	metric := StatsMetric{Samples: m.count, WindowSamples: m.windowSamples, WindowSamplesTruncated: m.truncated}
	if m.count > 0 {
		metric.EWMA = roundedPointer(m.ewma)
	}
	if m.windowSamples > 0 {
		metric.P50 = roundedPointer(m.p50)
		metric.P95 = roundedPointer(m.p95)
	}
	return metric
}

func modes(views [statsModes]metricView) StatsModes {
	return StatsModes{Stream: views[modeStream].metric(), NonStream: views[modeNonStream].metric()}
}

// routeStatsFor builds the stats array of a snapshot in routing order
// (Providers by descending priority, then Upstream Models by name).
func routeStatsFor(st *snapshot, now time.Time) []RouteStats {
	result := make([]RouteStats, 0, len(st.providers))
	for _, configured := range st.providers {
		for _, pair := range configured.upstreamRoutes() {
			entry := configured.stats[pair.upstream]
			if entry == nil {
				continue
			}
			view := entry.view(now)
			stats := RouteStats{
				Provider:      configured.Name,
				Upstream:      pair.upstream,
				VirtualModels: pair.virtuals,
				Enabled:       !configured.Disabled,
				Since:         formatStatsTime(view.since),
				Total:         view.total.counts(),
				Window:        view.window.counts(),
				TTFBMs:        modes(view.ttfb),
				TPS:           modes(view.tps),
				LastSuccessAt: optionalStatsTime(view.lastSuccess),
				LastFailureAt: optionalStatsTime(view.lastFailure),
			}
			if view.lastErrorClass != "" {
				class := view.lastErrorClass
				stats.LastErrorClass = &class
			}
			result = append(result, stats)
		}
	}
	return result
}
