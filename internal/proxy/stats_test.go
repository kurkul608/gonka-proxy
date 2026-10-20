package proxy

import (
	"math"
	"testing"
	"time"
)

var statsTestKey = statsKey{provider: "gate", baseURL: "http://gate.test/v1", upstream: "model"}

func newTestRegistry(t testing.TB, settings StatsSettings) *statsRegistry {
	t.Helper()
	registry := newStatsRegistry(settings)
	registry.sync([]statsKey{statsTestKey}, time.Unix(1_000_000, 0))
	return registry
}

func viewOf(t testing.TB, registry *statsRegistry, now time.Time) routeStatsView {
	t.Helper()
	view, ok := registry.view(now)[statsTestKey]
	if !ok {
		t.Fatal("test key missing from view")
	}
	return view
}

func success(at time.Time, ttfb time.Duration, tps float64) attemptRecord {
	return attemptRecord{outcome: outcomeSuccess, at: at, ttfb: ttfb, tokensPerSecond: tps}
}

func TestStatsSettingsDefaults(t *testing.T) {
	for _, settings := range []StatsSettings{{}, {Window: -time.Second, EWMAAlpha: -1}, {EWMAAlpha: 2}, {EWMAAlpha: math.NaN()}} {
		got := settings.normalized()
		if got.Window != DefaultStatsWindow || got.EWMAAlpha != DefaultStatsEWMAAlpha {
			t.Fatalf("normalized(%+v) = %+v", settings, got)
		}
	}
	if got := (StatsSettings{Window: time.Minute, EWMAAlpha: 1}).normalized(); got.Window != time.Minute || got.EWMAAlpha != 1 {
		t.Fatalf("explicit settings changed: %+v", got)
	}
}

func TestStatsEWMA(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{EWMAAlpha: 0.5})
	now := time.Unix(1_000_000, 0)
	registry.record(statsTestKey, success(now, 100*time.Millisecond, 10))
	registry.record(statsTestKey, success(now, 200*time.Millisecond, 30))
	registry.record(statsTestKey, success(now, 400*time.Millisecond, 50))

	view := viewOf(t, registry, now)
	// 100 -> 0.5*200+0.5*100 = 150 -> 0.5*400+0.5*150 = 275
	if view.ttfb.ewma != 275 {
		t.Fatalf("ttfb ewma = %v, want 275", view.ttfb.ewma)
	}
	// 10 -> 20 -> 35
	if view.tps.ewma != 35 {
		t.Fatalf("tps ewma = %v, want 35", view.tps.ewma)
	}
	if view.ttfb.count != 3 || view.tps.count != 3 {
		t.Fatalf("sample counts = %d, %d", view.ttfb.count, view.tps.count)
	}
}

func TestStatsSkipsMissingSamples(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{})
	now := time.Unix(1_000_000, 0)
	registry.record(statsTestKey, success(now, 0, 0))
	registry.record(statsTestKey, success(now, 0, math.Inf(1)))
	view := viewOf(t, registry, now)
	if view.total.successes != 2 || view.ttfb.count != 0 || view.tps.count != 0 {
		t.Fatalf("view = %+v", view)
	}
	if metric := view.ttfb.metric(); metric.EWMA != nil || metric.P50 != nil || metric.P95 != nil {
		t.Fatalf("metric without samples = %+v, want nulls", metric)
	}
}

func TestStatsPercentiles(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{})
	now := time.Unix(1_000_000, 0)
	// Recorded in reverse to prove the values are sorted.
	for value := 100; value >= 1; value-- {
		registry.record(statsTestKey, success(now, time.Duration(value)*time.Millisecond, float64(value)))
	}
	view := viewOf(t, registry, now)
	if view.ttfb.p50 != 50 || view.ttfb.p95 != 95 {
		t.Fatalf("ttfb p50/p95 = %v/%v, want 50/95", view.ttfb.p50, view.ttfb.p95)
	}
	if view.tps.p50 != 50 || view.tps.p95 != 95 || view.tps.windowSamples != 100 {
		t.Fatalf("tps p50/p95/n = %v/%v/%d", view.tps.p50, view.tps.p95, view.tps.windowSamples)
	}

	if got := percentile([]float64{7}, 0.95); got != 7 {
		t.Fatalf("single-sample p95 = %v", got)
	}
}

func TestStatsWindowExpiry(t *testing.T) {
	window := time.Minute
	registry := newTestRegistry(t, StatsSettings{Window: window})
	start := time.Unix(1_000_000, 0)
	registry.record(statsTestKey, success(start, 500*time.Millisecond, 10))
	registry.record(statsTestKey, attemptRecord{outcome: outcomeFailure, class: statsClassServer, at: start})

	later := start.Add(window / 2)
	registry.record(statsTestKey, success(later, 100*time.Millisecond, 20))

	view := viewOf(t, registry, later)
	if view.window.requests != 3 || view.window.server != 1 {
		t.Fatalf("window before expiry = %+v", view.window)
	}
	if view.ttfb.windowSamples != 2 {
		t.Fatalf("window samples before expiry = %d", view.ttfb.windowSamples)
	}

	// The first two attempts leave the window; the third stays.
	after := start.Add(window + window/statsBuckets)
	view = viewOf(t, registry, after)
	if view.window.requests != 1 || view.window.successes != 1 || view.window.failures != 0 {
		t.Fatalf("window after expiry = %+v", view.window)
	}
	if view.total.requests != 3 || view.total.failures != 1 || view.total.server != 1 {
		t.Fatalf("totals must not expire: %+v", view.total)
	}
	if view.ttfb.windowSamples != 1 || view.ttfb.p50 != 100 || view.ttfb.p95 != 100 {
		t.Fatalf("ttfb after expiry = %+v", view.ttfb)
	}
	if view.ttfb.count != 2 {
		t.Fatalf("ewma sample count = %d, want 2", view.ttfb.count)
	}

	// Everything leaves the window; a reused bucket starts over.
	empty := later.Add(2 * window)
	view = viewOf(t, registry, empty)
	if view.window.requests != 0 || view.ttfb.windowSamples != 0 {
		t.Fatalf("window long after = %+v / %+v", view.window, view.ttfb)
	}
	registry.record(statsTestKey, attemptRecord{outcome: outcomeFailure, class: statsClassTimeout, at: empty})
	view = viewOf(t, registry, empty)
	if view.window.requests != 1 || view.window.timeout != 1 || view.window.server != 0 {
		t.Fatalf("reused bucket kept stale counts: %+v", view.window)
	}
}

func TestStatsMemoryIsBounded(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{})
	now := time.Unix(1_000_000, 0)
	for index := 0; index < 5*statsSampleCap; index++ {
		at := now.Add(time.Duration(index) * time.Millisecond)
		registry.record(statsTestKey, success(at, time.Duration(index+1)*time.Microsecond, float64(index+1)))
	}
	entry := (*registry.entries.Load())[statsTestKey]
	if entry.ttfb.written != statsSampleCap || entry.tps.written != statsSampleCap {
		t.Fatalf("ring holds %d/%d samples, cap %d", entry.ttfb.written, entry.tps.written, statsSampleCap)
	}
	view := viewOf(t, registry, now.Add(time.Duration(5*statsSampleCap)*time.Millisecond))
	if view.tps.windowSamples != statsSampleCap {
		t.Fatalf("window samples = %d, want cap %d", view.tps.windowSamples, statsSampleCap)
	}
	// Only the newest samples are kept: the smallest kept value is
	// 5*cap - cap + 1.
	if view.total.requests != uint64(5*statsSampleCap) || view.tps.count != uint64(5*statsSampleCap) {
		t.Fatalf("totals = %+v", view.total)
	}
	if min := float64(4*statsSampleCap + 1); view.tps.p50 < min {
		t.Fatalf("p50 = %v includes evicted samples (< %v)", view.tps.p50, min)
	}
}

func TestStatsCancelIsNotAFailure(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{})
	now := time.Unix(1_000_000, 0)
	registry.record(statsTestKey, attemptRecord{outcome: outcomeCanceled, at: now})
	registry.record(statsTestKey, attemptRecord{outcome: outcomeClientError, at: now})

	view := viewOf(t, registry, now)
	for name, counters := range map[string]statsCounters{"total": view.total, "window": view.window} {
		if counters.requests != 2 || counters.canceled != 1 || counters.clientErrors != 1 || counters.failures != 0 || counters.successes != 0 {
			t.Fatalf("%s = %+v", name, counters)
		}
	}
	if !view.lastFailure.IsZero() || view.lastErrorClass != "" || !view.lastSuccess.IsZero() {
		t.Fatalf("cancel touched last success/failure: %+v", view)
	}
}

func TestStatsFailureClassesAndLastTimes(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{})
	now := time.Unix(1_000_000, 0)
	classes := []string{statsClassTimeout, statsClassServer, statsClassServer, statsClassClient, statsClassConnection}
	for index, class := range classes {
		registry.record(statsTestKey, attemptRecord{outcome: outcomeFailure, class: class, at: now.Add(time.Duration(index) * time.Second)})
	}
	registry.record(statsTestKey, success(now.Add(10*time.Second), time.Millisecond, 0))

	view := viewOf(t, registry, now.Add(10*time.Second))
	got := view.total.counts()
	want := FailuresByClass{Timeout: 1, Server: 2, Client: 1, Connection: 1}
	if got.Failures != 5 || got.FailuresByClass != want || got.Successes != 1 || got.Requests != 6 {
		t.Fatalf("counts = %+v", got)
	}
	if view.lastErrorClass != statsClassConnection || !view.lastFailure.Equal(now.Add(4*time.Second)) || !view.lastSuccess.Equal(now.Add(10*time.Second)) {
		t.Fatalf("last = %v %v %q", view.lastSuccess, view.lastFailure, view.lastErrorClass)
	}
}

func TestStatsSyncKeepsAndDropsKeys(t *testing.T) {
	registry := newTestRegistry(t, StatsSettings{})
	other := statsKey{provider: "other", baseURL: "http://other.test/v1", upstream: "model"}
	now := time.Unix(1_000_000, 0)
	registry.record(statsTestKey, success(now, time.Millisecond, 1))
	registry.record(other, success(now, time.Millisecond, 1)) // not configured: ignored

	if _, ok := registry.view(now)[other]; ok {
		t.Fatal("record created an unconfigured key")
	}
	registry.sync([]statsKey{statsTestKey, other}, now.Add(time.Second))
	views := registry.view(now)
	if views[statsTestKey].total.requests != 1 || views[other].total.requests != 0 {
		t.Fatalf("after sync: %+v", views)
	}
	if !views[other].since.Equal(now.Add(time.Second)) {
		t.Fatalf("new key since = %v", views[other].since)
	}
	registry.sync([]statsKey{other}, now)
	registry.record(statsTestKey, success(now, time.Millisecond, 1))
	if _, ok := registry.view(now)[statsTestKey]; ok {
		t.Fatal("a dropped key came back")
	}
}

func TestLastCompletionTokens(t *testing.T) {
	tests := map[string]int{
		`{"usage":{"prompt_tokens":5,"completion_tokens":42,"total_tokens":47}}`:                                                42,
		`{"usage":{"completion_tokens" : 7,"completion_tokens_details":{"reasoning_tokens":3}}}`:                                7,
		`data: {"usage":{"completion_tokens":1}}` + "\n\n" + `data: {"usage":{"completion_tokens":9}}` + "\n\ndata: [DONE]\n\n": 9,
		`{"content":"say \"completion_tokens\": 99","usage":null}`:                                                              0,
		`{"usage":{"completion_tokens":null}}`:                                                                                  0,
		`{"usage":{"completion_tokens":0}}`:                                                                                     0,
		`no usage here`:                                                                                                         0,
		`{"usage":{"completion_tokens":12`:                                                                                      0, // cut: the number may be incomplete
	}
	for input, want := range tests {
		if got := lastCompletionTokens([]byte(input)); got != want {
			t.Errorf("lastCompletionTokens(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestMeteredBodyKeepsBoundedTail(t *testing.T) {
	body := &meteredBody{}
	chunk := make([]byte, 1000)
	for index := range chunk {
		chunk[index] = 'x'
	}
	for index := 0; index < 100; index++ {
		body.keep(chunk)
	}
	body.keep([]byte(`{"usage":{"completion_tokens":5}}`))
	if len(body.tail) != usageTailBytes {
		t.Fatalf("tail length = %d, want %d", len(body.tail), usageTailBytes)
	}
	if got := body.completionTokens(); got != 5 {
		t.Fatalf("completion tokens = %d", got)
	}
	big := make([]byte, 2*usageTailBytes)
	body.keep(big)
	if len(body.tail) != usageTailBytes {
		t.Fatalf("tail after big chunk = %d", len(body.tail))
	}
}

func BenchmarkStatsRecord(b *testing.B) {
	registry := newTestRegistry(b, StatsSettings{})
	start := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := 0
		for pb.Next() {
			index++
			registry.record(statsTestKey, success(start.Add(time.Duration(index)*time.Microsecond), 250*time.Millisecond, 42))
		}
	})
}

func BenchmarkStatsRecordManyKeys(b *testing.B) {
	registry := newStatsRegistry(StatsSettings{})
	keys := make([]statsKey, 64)
	for index := range keys {
		keys[index] = statsKey{provider: "gate", baseURL: "http://gate.test/v1", upstream: string(rune('a'+index%26)) + string(rune('a'+index/26))}
	}
	registry.sync(keys, time.Now())
	start := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := 0
		for pb.Next() {
			index++
			registry.record(keys[index%len(keys)], success(start, 250*time.Millisecond, 42))
		}
	})
}
