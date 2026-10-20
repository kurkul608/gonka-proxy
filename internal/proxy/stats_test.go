package proxy

import (
	"math"
	"sync"
	"testing"
	"time"
)

var statsTestKey = statsKey{provider: "gate", baseURL: "http://gate.test/v1", upstream: "model"}

var statsEpoch = time.Unix(1_000_000, 0)

func newTestEntry(t testing.TB, settings StatsSettings) *routeStats {
	t.Helper()
	registry := newStatsRegistry(settings)
	return registry.ensure([]statsKey{statsTestKey}, statsEpoch)[statsTestKey]
}

func success(at time.Time, ttfb time.Duration, tps float64) attemptRecord {
	return attemptRecord{outcome: outcomeSuccess, at: at, ttfb: ttfb, tokensPerSecond: tps}
}

func streamSuccess(at time.Time, ttfb time.Duration, tps float64) attemptRecord {
	record := success(at, ttfb, tps)
	record.streaming = true
	return record
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
	entry := newTestEntry(t, StatsSettings{EWMAAlpha: 0.5})
	entry.record(success(statsEpoch, 100*time.Millisecond, 10))
	entry.record(success(statsEpoch, 200*time.Millisecond, 30))
	entry.record(success(statsEpoch, 400*time.Millisecond, 50))

	view := entry.view(statsEpoch)
	ttfb, tps := view.ttfb[modeNonStream], view.tps[modeNonStream]
	// 100 -> 0.5*200+0.5*100 = 150 -> 0.5*400+0.5*150 = 275
	if ttfb.ewma != 275 {
		t.Fatalf("ttfb ewma = %v, want 275", ttfb.ewma)
	}
	// 10 -> 20 -> 35
	if tps.ewma != 35 {
		t.Fatalf("tps ewma = %v, want 35", tps.ewma)
	}
	if ttfb.count != 3 || tps.count != 3 {
		t.Fatalf("sample counts = %d, %d", ttfb.count, tps.count)
	}
}

func TestStatsSplitStreamAndNonStream(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{})
	entry.record(success(statsEpoch, 1000*time.Millisecond, 10))
	entry.record(streamSuccess(statsEpoch, 200*time.Millisecond, 40))
	entry.record(streamSuccess(statsEpoch, 200*time.Millisecond, 40))

	view := entry.view(statsEpoch)
	if got := view.ttfb[modeNonStream]; got.count != 1 || got.ewma != 1000 || got.p50 != 1000 {
		t.Fatalf("non-stream ttfb = %+v", got)
	}
	if got := view.ttfb[modeStream]; got.count != 2 || got.ewma != 200 || got.p95 != 200 {
		t.Fatalf("stream ttfb = %+v", got)
	}
	if view.tps[modeNonStream].ewma != 10 || view.tps[modeStream].ewma != 40 {
		t.Fatalf("tps = %+v", view.tps)
	}
	if view.total.successes != 3 {
		t.Fatalf("counters are not split by mode: %+v", view.total)
	}
	json := modes(view.ttfb)
	if json.Stream.Samples != 2 || json.NonStream.Samples != 1 {
		t.Fatalf("modes() = %+v", json)
	}
}

func TestStatsSkipsMissingSamples(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{})
	entry.record(success(statsEpoch, 0, 0))
	entry.record(success(statsEpoch, 0, math.Inf(1)))
	view := entry.view(statsEpoch)
	for mode := 0; mode < statsModes; mode++ {
		if view.ttfb[mode].count != 0 || view.tps[mode].count != 0 {
			t.Fatalf("samples recorded without values: %+v", view)
		}
		if metric := view.ttfb[mode].metric(); metric.EWMA != nil || metric.P50 != nil || metric.P95 != nil {
			t.Fatalf("metric without samples = %+v, want nulls", metric)
		}
	}
	if view.total.successes != 2 {
		t.Fatalf("successes = %d", view.total.successes)
	}
}

func TestStatsPercentiles(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{})
	// Recorded in reverse to prove the values are sorted.
	for value := 100; value >= 1; value-- {
		entry.record(success(statsEpoch, time.Duration(value)*time.Millisecond, float64(value)))
	}
	view := entry.view(statsEpoch)
	if got := view.ttfb[modeNonStream]; got.p50 != 50 || got.p95 != 95 {
		t.Fatalf("ttfb p50/p95 = %v/%v, want 50/95", got.p50, got.p95)
	}
	if got := view.tps[modeNonStream]; got.p50 != 50 || got.p95 != 95 || got.windowSamples != 100 || got.truncated {
		t.Fatalf("tps = %+v", got)
	}
	if got := percentile([]float64{7}, 0.95); got != 7 {
		t.Fatalf("single-sample p95 = %v", got)
	}
}

func TestStatsWindowExpiry(t *testing.T) {
	window := time.Minute
	entry := newTestEntry(t, StatsSettings{Window: window})
	start := statsEpoch
	entry.record(success(start, 500*time.Millisecond, 10))
	entry.record(attemptRecord{outcome: outcomeFailure, class: statsClassServer, at: start})

	later := start.Add(window / 2)
	entry.record(success(later, 100*time.Millisecond, 20))

	view := entry.view(later)
	if view.window.requests != 3 || view.window.server != 1 {
		t.Fatalf("window before expiry = %+v", view.window)
	}
	if view.ttfb[modeNonStream].windowSamples != 2 {
		t.Fatalf("window samples before expiry = %d", view.ttfb[modeNonStream].windowSamples)
	}

	// The first two attempts leave the window; the third stays.
	after := start.Add(window + window/statsBuckets)
	view = entry.view(after)
	if view.window.requests != 1 || view.window.successes != 1 || view.window.failures != 0 {
		t.Fatalf("window after expiry = %+v", view.window)
	}
	if view.total.requests != 3 || view.total.failures != 1 || view.total.server != 1 {
		t.Fatalf("totals must not expire: %+v", view.total)
	}
	ttfb := view.ttfb[modeNonStream]
	if ttfb.windowSamples != 1 || ttfb.p50 != 100 || ttfb.p95 != 100 || ttfb.count != 2 {
		t.Fatalf("ttfb after expiry = %+v", ttfb)
	}

	// Everything leaves the window; a reused bucket starts over.
	empty := later.Add(2 * window)
	view = entry.view(empty)
	if view.window.requests != 0 || view.ttfb[modeNonStream].windowSamples != 0 {
		t.Fatalf("window long after = %+v / %+v", view.window, view.ttfb)
	}
	entry.record(attemptRecord{outcome: outcomeFailure, class: statsClassTimeout, at: empty})
	view = entry.view(empty)
	if view.window.requests != 1 || view.window.timeout != 1 || view.window.server != 0 {
		t.Fatalf("reused bucket kept stale counts: %+v", view.window)
	}
}

func TestStatsMemoryIsBoundedAndTruncationIsReported(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{Window: time.Hour})
	for index := 0; index < 5*statsSampleCap; index++ {
		at := statsEpoch.Add(time.Duration(index) * time.Millisecond)
		entry.record(success(at, time.Duration(index+1)*time.Microsecond, float64(index+1)))
	}
	ring := &entry.tps[modeNonStream]
	if ring.written != statsSampleCap || entry.ttfb[modeNonStream].written != statsSampleCap {
		t.Fatalf("ring holds %d samples, cap %d", ring.written, statsSampleCap)
	}
	now := statsEpoch.Add(time.Duration(5*statsSampleCap) * time.Millisecond)
	view := entry.view(now)
	tps := view.tps[modeNonStream]
	if tps.windowSamples != statsSampleCap || !tps.truncated {
		t.Fatalf("window samples = %d truncated = %v, want cap %d and true", tps.windowSamples, tps.truncated, statsSampleCap)
	}
	if view.total.requests != uint64(5*statsSampleCap) || tps.count != uint64(5*statsSampleCap) {
		t.Fatalf("totals = %+v", view.total)
	}
	// Only the newest samples are kept.
	if min := float64(4*statsSampleCap + 1); tps.p50 < min {
		t.Fatalf("p50 = %v includes evicted samples (< %v)", tps.p50, min)
	}

	// Once the oldest kept sample leaves the window, nothing in the window
	// was evicted.
	view = entry.view(statsEpoch.Add(time.Hour + time.Duration(4*statsSampleCap+10)*time.Millisecond))
	if got := view.tps[modeNonStream]; got.truncated || got.windowSamples >= statsSampleCap {
		t.Fatalf("after the oldest sample expired: %+v", got)
	}
}

func TestStatsCancelClientErrorAndDeadlineAreNotFailures(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{})
	entry.record(attemptRecord{outcome: outcomeCanceled, at: statsEpoch})
	entry.record(attemptRecord{outcome: outcomeClientError, at: statsEpoch})
	entry.record(attemptRecord{outcome: outcomeDeadline, at: statsEpoch})

	view := entry.view(statsEpoch)
	for name, counters := range map[string]statsCounters{"total": view.total, "window": view.window} {
		if counters.requests != 3 || counters.canceled != 1 || counters.clientErrors != 1 || counters.deadline != 1 ||
			counters.failures != 0 || counters.successes != 0 || counters.timeout != 0 {
			t.Fatalf("%s = %+v", name, counters)
		}
	}
	if !view.lastFailure.IsZero() || view.lastErrorClass != "" || !view.lastSuccess.IsZero() {
		t.Fatalf("non-failures touched last success/failure: %+v", view)
	}
	if got := view.total.counts(); got.Deadline != 1 || got.Failures != 0 {
		t.Fatalf("counts() = %+v", got)
	}
}

func TestStatsFailureClassesAndLastTimes(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{})
	classes := []string{statsClassTimeout, statsClassServer, statsClassServer, statsClassClient, statsClassConnection}
	for index, class := range classes {
		entry.record(attemptRecord{outcome: outcomeFailure, class: class, at: statsEpoch.Add(time.Duration(index) * time.Second)})
	}
	entry.record(success(statsEpoch.Add(10*time.Second), time.Millisecond, 0))

	view := entry.view(statsEpoch.Add(10 * time.Second))
	got := view.total.counts()
	want := FailuresByClass{Timeout: 1, Server: 2, Client: 1, Connection: 1}
	if got.Failures != 5 || got.FailuresByClass != want || got.Successes != 1 || got.Requests != 6 {
		t.Fatalf("counts = %+v", got)
	}
	if view.lastErrorClass != statsClassConnection || !view.lastFailure.Equal(statsEpoch.Add(4*time.Second)) || !view.lastSuccess.Equal(statsEpoch.Add(10*time.Second)) {
		t.Fatalf("last = %v %v %q", view.lastSuccess, view.lastFailure, view.lastErrorClass)
	}
}

func TestPassThroughRecord(t *testing.T) {
	tests := map[int]attemptRecord{
		401: {outcome: outcomeFailure, class: statsClassClient},
		403: {outcome: outcomeFailure, class: statsClassClient},
		404: {outcome: outcomeFailure, class: statsClassClient},
		400: {outcome: outcomeClientError},
		422: {outcome: outcomeClientError},
		413: {outcome: outcomeClientError},
		201: {outcome: outcomeSuccess},
	}
	for status, want := range tests {
		if got := passThroughRecord(status); got != want {
			t.Errorf("passThroughRecord(%d) = %+v, want %+v", status, got, want)
		}
	}
}

func TestStatsRegistryEnsureAndRetain(t *testing.T) {
	registry := newStatsRegistry(StatsSettings{})
	other := statsKey{provider: "other", baseURL: "http://other.test/v1", upstream: "model"}
	first := registry.ensure([]statsKey{statsTestKey}, statsEpoch)[statsTestKey]
	first.record(success(statsEpoch, time.Millisecond, 1))

	// Adding a key keeps the existing entry.
	resolved := registry.ensure([]statsKey{statsTestKey, other}, statsEpoch.Add(time.Second))
	if resolved[statsTestKey] != first {
		t.Fatal("ensure replaced a live entry")
	}
	if !resolved[other].view(statsEpoch).since.Equal(statsEpoch.Add(time.Second)) {
		t.Fatal("new entry has the wrong since")
	}

	// Dropping a key detaches its entry; re-adding starts clean.
	registry.retain([]statsKey{other})
	readded := registry.ensure([]statsKey{statsTestKey}, statsEpoch)[statsTestKey]
	if readded == first || readded.view(statsEpoch).total.requests != 0 {
		t.Fatal("a re-added key inherited the dropped entry")
	}
	first.record(success(statsEpoch, time.Millisecond, 1)) // detached: harmless
	if readded.view(statsEpoch).total.requests != 0 {
		t.Fatal("a detached entry leaked into the new one")
	}

	var nilEntry *routeStats
	nilEntry.record(success(statsEpoch, time.Millisecond, 1)) // must not panic
}

func TestStatsViewWhileRecording(t *testing.T) {
	entry := newTestEntry(t, StatsSettings{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(streaming bool) {
			defer wg.Done()
			for index := 0; ; index++ {
				select {
				case <-stop:
					return
				default:
				}
				record := success(time.Now(), time.Duration(index%500+1)*time.Millisecond, float64(index%90+1))
				record.streaming = streaming
				entry.record(record)
			}
		}(worker%2 == 0)
	}
	for index := 0; index < 200; index++ {
		_ = entry.view(time.Now())
	}
	close(stop)
	wg.Wait()
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

// BenchmarkStatsRecord measures the hot path: one record through the
// route's resolved pointer, all goroutines on one route.
func BenchmarkStatsRecord(b *testing.B) {
	entry := newTestEntry(b, StatsSettings{})
	candidate := route{upstream: "model", stats: entry}
	server := &Server{}
	start := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := 0
		for pb.Next() {
			index++
			server.recordAttempt(candidate, streamSuccess(start.Add(time.Duration(index)*time.Microsecond), 250*time.Millisecond, 42))
		}
	})
}

func BenchmarkStatsRecordManyRoutes(b *testing.B) {
	registry := newStatsRegistry(StatsSettings{})
	keys := make([]statsKey, 64)
	for index := range keys {
		keys[index] = statsKey{provider: "gate", baseURL: "http://gate.test/v1", upstream: string(rune('a'+index%26)) + string(rune('a'+index/26))}
	}
	resolved := registry.ensure(keys, time.Now())
	routes := make([]route, len(keys))
	for index, key := range keys {
		routes[index] = route{upstream: key.upstream, stats: resolved[key]}
	}
	server := &Server{}
	start := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := 0
		for pb.Next() {
			index++
			server.recordAttempt(routes[index%len(routes)], success(start, 250*time.Millisecond, 42))
		}
	})
}
