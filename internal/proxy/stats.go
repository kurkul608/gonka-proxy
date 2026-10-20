package proxy

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultStatsWindow is the sliding window for windowed counters and
	// percentiles.
	DefaultStatsWindow = 15 * time.Minute
	// DefaultStatsEWMAAlpha weighs the newest sample in the TTFB and
	// tokens/s moving averages.
	DefaultStatsEWMAAlpha = 0.2

	// statsBuckets splits the window into time buckets for the windowed
	// counters: the window covered is between window*(1-1/statsBuckets) and
	// window.
	statsBuckets = 60
	// statsSampleCap bounds the samples kept per metric and key for the
	// windowed percentiles. With more samples than this inside the window,
	// percentiles describe the most recent statsSampleCap of them.
	statsSampleCap = 1024
)

// Failure classes of a upstream attempt. A client cancel is not a failure.
const (
	statsClassTimeout    = "timeout"
	statsClassServer     = "5xx"
	statsClassClient     = "4xx"
	statsClassConnection = "connection"
)

// attemptOutcome is how one upstream attempt ended.
type attemptOutcome int

const (
	outcomeSuccess attemptOutcome = iota
	outcomeFailure
	// outcomeCanceled: the client went away; not the Provider's fault.
	outcomeCanceled
	// outcomeClientError: a 4xx passed back to the client unchanged (the
	// request was bad, not the Provider).
	outcomeClientError
)

// attemptRecord is one finished attempt as seen by the stats registry.
type attemptRecord struct {
	outcome attemptOutcome
	class   string // failure class when outcome == outcomeFailure
	at      time.Time
	// ttfb is the time to the first upstream body byte; zero means no sample.
	ttfb time.Duration
	// tokensPerSecond is the output rate from upstream usage; zero means no sample.
	tokensPerSecond float64
}

// statsKey addresses the stats of one (Provider, Upstream Model) pair. The
// API key is not part of it: rotating a key keeps the route's history. A new
// name or base URL is a different route.
type statsKey struct {
	provider string
	baseURL  string
	upstream string
}

// StatsSettings tune the per-route statistics. Zero values take the defaults.
type StatsSettings struct {
	Window    time.Duration
	EWMAAlpha float64
}

func (s StatsSettings) normalized() StatsSettings {
	if s.Window <= 0 {
		s.Window = DefaultStatsWindow
	}
	if s.EWMAAlpha <= 0 || s.EWMAAlpha > 1 || math.IsNaN(s.EWMAAlpha) {
		s.EWMAAlpha = DefaultStatsEWMAAlpha
	}
	return s
}

// statsRegistry holds the stats of every configured (Provider, Upstream
// Model). The key set changes only on (re)configuration, so the map is
// copy-on-write behind an atomic pointer: the record path never takes a
// registry-wide lock.
type statsRegistry struct {
	settings StatsSettings
	bucket   time.Duration

	syncMu  sync.Mutex
	entries atomic.Pointer[map[statsKey]*routeStats]
}

func newStatsRegistry(settings StatsSettings) *statsRegistry {
	settings = settings.normalized()
	bucket := settings.Window / statsBuckets
	if bucket <= 0 {
		bucket = 1
	}
	registry := &statsRegistry{settings: settings, bucket: bucket}
	empty := map[statsKey]*routeStats{}
	registry.entries.Store(&empty)
	return registry
}

// sync makes the key set match keys at time now: entries for removed keys
// are dropped, kept ones keep their state, new ones start empty.
func (r *statsRegistry) sync(keys []statsKey, now time.Time) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	current := *r.entries.Load()
	next := make(map[statsKey]*routeStats, len(keys))
	for _, key := range keys {
		if entry, ok := current[key]; ok {
			next[key] = entry
			continue
		}
		next[key] = newRouteStats(now)
	}
	r.entries.Store(&next)
}

// record adds one attempt. Attempts for keys that are not configured (for
// example from a request still running on a snapshot whose route a reload
// removed) are ignored, so removed routes do not come back.
func (r *statsRegistry) record(key statsKey, attempt attemptRecord) {
	entry := (*r.entries.Load())[key]
	if entry == nil {
		return
	}
	entry.record(r, attempt)
}

// view returns a copy of every entry's aggregates at time now.
func (r *statsRegistry) view(now time.Time) map[statsKey]routeStatsView {
	entries := *r.entries.Load()
	views := make(map[statsKey]routeStatsView, len(entries))
	for key, entry := range entries {
		views[key] = entry.view(r, now)
	}
	return views
}

// routeStats is the state of one key. Its mutex is held only for in-memory
// updates, never across I/O.
type routeStats struct {
	mu sync.Mutex

	since   time.Time
	total   statsCounters
	buckets [statsBuckets]statsBucket

	ttfb ewmaRing
	tps  ewmaRing

	lastSuccess    time.Time
	lastFailure    time.Time
	lastErrorClass string
}

func newRouteStats(now time.Time) *routeStats {
	return &routeStats{since: now}
}

type statsCounters struct {
	requests     uint64
	successes    uint64
	failures     uint64
	canceled     uint64
	clientErrors uint64
	timeout      uint64
	server       uint64
	client       uint64
	connection   uint64
}

func (c *statsCounters) add(attempt attemptRecord) {
	c.requests++
	switch attempt.outcome {
	case outcomeSuccess:
		c.successes++
	case outcomeCanceled:
		c.canceled++
	case outcomeClientError:
		c.clientErrors++
	case outcomeFailure:
		c.failures++
		switch attempt.class {
		case statsClassTimeout:
			c.timeout++
		case statsClassServer:
			c.server++
		case statsClassClient:
			c.client++
		default:
			c.connection++
		}
	}
}

func (c *statsCounters) merge(other statsCounters) {
	c.requests += other.requests
	c.successes += other.successes
	c.failures += other.failures
	c.canceled += other.canceled
	c.clientErrors += other.clientErrors
	c.timeout += other.timeout
	c.server += other.server
	c.client += other.client
	c.connection += other.connection
}

// statsBucket holds the counters of one time slice of the window. epoch is
// the slice number (time / bucket width); a stale epoch means the bucket is
// reused and starts over.
type statsBucket struct {
	epoch    int64
	counters statsCounters
}

// ewmaRing keeps a moving average and a bounded ring of timestamped samples
// for windowed percentiles.
type ewmaRing struct {
	ewma    float64
	count   uint64 // samples since start
	values  [statsSampleCap]float64
	stamps  [statsSampleCap]int64 // unix nanoseconds
	next    int
	written int
}

func (e *ewmaRing) add(value float64, at time.Time, alpha float64) {
	if e.count == 0 {
		e.ewma = value
	} else {
		e.ewma = alpha*value + (1-alpha)*e.ewma
	}
	e.count++
	e.values[e.next] = value
	e.stamps[e.next] = at.UnixNano()
	e.next = (e.next + 1) % statsSampleCap
	if e.written < statsSampleCap {
		e.written++
	}
}

// windowValues returns the samples newer than cutoff, sorted.
func (e *ewmaRing) windowValues(cutoff int64) []float64 {
	values := make([]float64, 0, e.written)
	for index := 0; index < e.written; index++ {
		if e.stamps[index] > cutoff {
			values = append(values, e.values[index])
		}
	}
	sort.Float64s(values)
	return values
}

func (g *routeStats) record(r *statsRegistry, attempt attemptRecord) {
	epoch := attempt.at.UnixNano() / int64(r.bucket)
	slot := int(epoch % statsBuckets)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.total.add(attempt)
	bucket := &g.buckets[slot]
	if bucket.epoch != epoch {
		*bucket = statsBucket{epoch: epoch}
	}
	bucket.counters.add(attempt)

	switch attempt.outcome {
	case outcomeSuccess:
		g.lastSuccess = attempt.at
	case outcomeFailure:
		g.lastFailure = attempt.at
		g.lastErrorClass = attempt.class
	}
	if attempt.ttfb > 0 {
		g.ttfb.add(float64(attempt.ttfb)/float64(time.Millisecond), attempt.at, r.settings.EWMAAlpha)
	}
	if attempt.tokensPerSecond > 0 && !math.IsInf(attempt.tokensPerSecond, 0) && !math.IsNaN(attempt.tokensPerSecond) {
		g.tps.add(attempt.tokensPerSecond, attempt.at, r.settings.EWMAAlpha)
	}
}

// routeStatsView is a point-in-time copy of one entry's aggregates.
type routeStatsView struct {
	since          time.Time
	total          statsCounters
	window         statsCounters
	ttfb           metricView
	tps            metricView
	lastSuccess    time.Time
	lastFailure    time.Time
	lastErrorClass string
}

type metricView struct {
	ewma          float64
	count         uint64
	windowSamples int
	p50, p95      float64
}

func (g *routeStats) view(r *statsRegistry, now time.Time) routeStatsView {
	nowEpoch := now.UnixNano() / int64(r.bucket)
	cutoff := now.Add(-r.settings.Window).UnixNano()

	g.mu.Lock()
	view := routeStatsView{
		since:          g.since,
		total:          g.total,
		lastSuccess:    g.lastSuccess,
		lastFailure:    g.lastFailure,
		lastErrorClass: g.lastErrorClass,
	}
	for index := range g.buckets {
		bucket := &g.buckets[index]
		if bucket.epoch > nowEpoch-statsBuckets && bucket.epoch <= nowEpoch {
			view.window.merge(bucket.counters)
		}
	}
	ttfbValues := g.ttfb.windowValues(cutoff)
	tpsValues := g.tps.windowValues(cutoff)
	view.ttfb = metricView{ewma: g.ttfb.ewma, count: g.ttfb.count}
	view.tps = metricView{ewma: g.tps.ewma, count: g.tps.count}
	g.mu.Unlock()

	view.ttfb.fill(ttfbValues)
	view.tps.fill(tpsValues)
	return view
}

func (m *metricView) fill(sorted []float64) {
	m.windowSamples = len(sorted)
	if len(sorted) == 0 {
		return
	}
	m.p50 = percentile(sorted, 0.50)
	m.p95 = percentile(sorted, 0.95)
}

// percentile is the nearest-rank percentile of an ascending, non-empty slice.
func percentile(sorted []float64, p float64) float64 {
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
