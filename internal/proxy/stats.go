package proxy

import (
	"math"
	"sort"
	"sync"
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
	// statsSampleCap bounds the samples kept per metric, mode and route for
	// the windowed percentiles. With more samples than this inside the
	// window, percentiles describe the most recent statsSampleCap of them
	// (window_samples_truncated).
	statsSampleCap = 1024
)

// Failure classes of an upstream attempt. A client cancel, a Client Error
// and a routing deadline are not failures.
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
	// outcomeClientError: a client-caused 4xx (400, 422, ...) passed back to
	// the client unchanged.
	outcomeClientError
	// outcomeDeadline: the request's routing deadline ran out while this
	// attempt was in flight, either because the client chose a short
	// X-Gonka-Max-Routing-Time or because the deadline won a race with the
	// response. Not the Provider's fault.
	outcomeDeadline
)

// Response modes for the TTFB and tokens/s metrics.
const (
	modeNonStream = 0
	modeStream    = 1
	statsModes    = 2
)

// attemptRecord is one finished attempt as seen by the stats.
type attemptRecord struct {
	outcome   attemptOutcome
	class     string // failure class when outcome == outcomeFailure
	at        time.Time
	streaming bool
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

// statsConfig is shared, read-only configuration of every routeStats.
type statsConfig struct {
	window time.Duration
	alpha  float64
	bucket time.Duration
}

// statsRegistry keeps routeStats alive across reloads. It is touched only
// when a snapshot is installed; requests record through the pointers their
// snapshot resolved at build time, so a route a reload removed keeps
// recording into a detached entry that dies with its snapshot.
type statsRegistry struct {
	cfg *statsConfig

	mu      sync.Mutex
	entries map[statsKey]*routeStats
}

func newStatsRegistry(settings StatsSettings) *statsRegistry {
	settings = settings.normalized()
	bucket := settings.Window / statsBuckets
	if bucket <= 0 {
		bucket = 1
	}
	return &statsRegistry{
		cfg:     &statsConfig{window: settings.Window, alpha: settings.EWMAAlpha, bucket: bucket},
		entries: make(map[statsKey]*routeStats),
	}
}

func (r *statsRegistry) settings() StatsSettings {
	return StatsSettings{Window: r.cfg.window, EWMAAlpha: r.cfg.alpha}
}

// ensure returns the entries for keys, creating missing ones as of now.
// Existing entries keep their state.
func (r *statsRegistry) ensure(keys []statsKey, now time.Time) map[statsKey]*routeStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved := make(map[statsKey]*routeStats, len(keys))
	for _, key := range keys {
		entry := r.entries[key]
		if entry == nil {
			entry = newRouteStats(r.cfg, now)
			r.entries[key] = entry
		}
		resolved[key] = entry
	}
	return resolved
}

// retain drops every entry whose key is not in keys.
func (r *statsRegistry) retain(keys []statsKey) {
	keep := make(map[statsKey]struct{}, len(keys))
	for _, key := range keys {
		keep[key] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.entries {
		if _, ok := keep[key]; !ok {
			delete(r.entries, key)
		}
	}
}

// routeStats is the state of one route. Its mutex is held only for
// in-memory updates and copies, never across I/O.
type routeStats struct {
	cfg *statsConfig

	mu      sync.Mutex
	since   time.Time
	total   statsCounters
	buckets [statsBuckets]statsBucket

	ttfb [statsModes]ewmaRing
	tps  [statsModes]ewmaRing

	lastSuccess    time.Time
	lastFailure    time.Time
	lastErrorClass string
}

func newRouteStats(cfg *statsConfig, now time.Time) *routeStats {
	return &routeStats{cfg: cfg, since: now}
}

type statsCounters struct {
	requests     uint64
	successes    uint64
	failures     uint64
	canceled     uint64
	clientErrors uint64
	deadline     uint64
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
	case outcomeDeadline:
		c.deadline++
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
	c.deadline += other.deadline
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

// ringCopy is a copy of a ring taken under the lock, so filtering and
// sorting run after Unlock.
type ringCopy struct {
	ewma   float64
	count  uint64
	values []float64
	stamps []int64
	full   bool
	oldest int64 // stamp of the oldest kept sample when the ring is full
}

func (e *ewmaRing) copyOut() ringCopy {
	out := ringCopy{
		ewma:   e.ewma,
		count:  e.count,
		values: append([]float64(nil), e.values[:e.written]...),
		stamps: append([]int64(nil), e.stamps[:e.written]...),
		full:   e.written == statsSampleCap,
	}
	if out.full {
		out.oldest = e.stamps[e.next]
	}
	return out
}

// record adds one attempt. A nil receiver (a route without stats) is a no-op.
func (g *routeStats) record(attempt attemptRecord) {
	if g == nil {
		return
	}
	cfg := g.cfg
	epoch := attempt.at.UnixNano() / int64(cfg.bucket)
	slot := int(epoch % statsBuckets)
	mode := modeNonStream
	if attempt.streaming {
		mode = modeStream
	}

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
		g.ttfb[mode].add(float64(attempt.ttfb)/float64(time.Millisecond), attempt.at, cfg.alpha)
	}
	if attempt.tokensPerSecond > 0 && !math.IsInf(attempt.tokensPerSecond, 0) && !math.IsNaN(attempt.tokensPerSecond) {
		g.tps[mode].add(attempt.tokensPerSecond, attempt.at, cfg.alpha)
	}
}

// routeStatsView is a point-in-time copy of one entry's aggregates.
type routeStatsView struct {
	since          time.Time
	total          statsCounters
	window         statsCounters
	ttfb           [statsModes]metricView
	tps            [statsModes]metricView
	lastSuccess    time.Time
	lastFailure    time.Time
	lastErrorClass string
}

type metricView struct {
	ewma          float64
	count         uint64
	windowSamples int
	truncated     bool
	p50, p95      float64
}

func (g *routeStats) view(now time.Time) routeStatsView {
	cfg := g.cfg
	nowEpoch := now.UnixNano() / int64(cfg.bucket)
	cutoff := now.Add(-cfg.window).UnixNano()

	var ttfb, tps [statsModes]ringCopy
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
	for mode := 0; mode < statsModes; mode++ {
		ttfb[mode] = g.ttfb[mode].copyOut()
		tps[mode] = g.tps[mode].copyOut()
	}
	g.mu.Unlock()

	for mode := 0; mode < statsModes; mode++ {
		view.ttfb[mode] = ttfb[mode].metric(cutoff)
		view.tps[mode] = tps[mode].metric(cutoff)
	}
	return view
}

// metric filters the copied samples to the window and computes percentiles.
func (c ringCopy) metric(cutoff int64) metricView {
	view := metricView{ewma: c.ewma, count: c.count}
	values := make([]float64, 0, len(c.values))
	for index, stamp := range c.stamps {
		if stamp > cutoff {
			values = append(values, c.values[index])
		}
	}
	sort.Float64s(values)
	view.windowSamples = len(values)
	// A full ring whose oldest sample is still inside the window may have
	// evicted older in-window samples.
	view.truncated = c.full && c.oldest > cutoff
	if len(values) > 0 {
		view.p50 = percentile(values, 0.50)
		view.p95 = percentile(values, 0.95)
	}
	return view
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
