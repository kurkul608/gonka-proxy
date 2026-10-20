package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

func routeStats(t *testing.T, handler *proxy.Server, provider, upstream string) proxy.RouteStats {
	t.Helper()
	for _, entry := range handler.Status().Stats {
		if entry.Provider == provider && entry.Upstream == upstream {
			return entry
		}
	}
	t.Fatalf("no stats for %s / %s", provider, upstream)
	return proxy.RouteStats{}
}

func hasRouteStats(handler *proxy.Server, provider, upstream string) bool {
	for _, entry := range handler.Status().Stats {
		if entry.Provider == provider && entry.Upstream == upstream {
			return true
		}
	}
	return false
}

// waitForRequests polls until the gate has seen want attempts: the proxy
// records an attempt after the client may already have its answer.
func waitForRequests(t *testing.T, handler *proxy.Server, provider, upstream string, want uint64) proxy.RouteStats {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		entry := routeStats(t, handler, provider, upstream)
		if entry.Total.Requests >= want || time.Now().After(deadline) {
			return entry
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const completionWithUsage = `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":20,"total_tokens":23}}`

func TestStatsMeasureTTFBOfSlowFirstByte(t *testing.T) {
	const delay = 150 * time.Millisecond
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers now, body later
		time.Sleep(delay)
		_, _ = io.WriteString(w, completionWithUsage)
	}))
	defer slow.Close()
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "slow", BaseURL: slow.URL, APIKey: "key-slow-0123456789", Priority: 1, Models: map[string]string{"chat": "slow-chat"}},
	), proxy.Options{})

	if response := postChat(t, server, chatBody("chat"), nil); response.status != http.StatusOK {
		t.Fatalf("status %d: %s", response.status, response.body)
	}
	entry := waitForRequests(t, handler, "slow", "slow-chat", 1)
	if entry.Total.Successes != 1 || entry.Total.Failures != 0 {
		t.Fatalf("counts = %+v", entry.Total)
	}
	if entry.TTFBMs.Samples != 1 || entry.TTFBMs.EWMA == nil || entry.TTFBMs.P50 == nil || entry.TTFBMs.P95 == nil {
		t.Fatalf("ttfb = %+v", entry.TTFBMs)
	}
	if got := *entry.TTFBMs.EWMA; got < float64(delay/time.Millisecond) || got > 2000 {
		t.Fatalf("ttfb = %vms, want >= %v", got, delay)
	}
	if *entry.TTFBMs.P50 != *entry.TTFBMs.EWMA || *entry.TTFBMs.P95 != *entry.TTFBMs.EWMA {
		t.Fatalf("one sample: p50/p95 must equal it: %+v", entry.TTFBMs)
	}
	// 20 tokens in at least 150ms.
	if entry.TPS.Samples != 1 || entry.TPS.EWMA == nil || *entry.TPS.EWMA <= 0 || *entry.TPS.EWMA > 20/delay.Seconds() {
		t.Fatalf("tokens/s = %+v", entry.TPS)
	}
	if entry.LastSuccessAt == nil || entry.LastFailureAt != nil || entry.LastErrorClass != nil {
		t.Fatalf("last = %v %v %v", entry.LastSuccessAt, entry.LastFailureAt, entry.LastErrorClass)
	}
	if strings.Join(entry.VirtualModels, ",") != "chat" || !entry.Enabled {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestStatsSkipTokensWithoutUsage(t *testing.T) {
	upstream := newFakeUpstream(t) // answers without usage
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: upstream.URL, APIKey: "key-a", Priority: 1, ModelAlias: "a-model"},
	), proxy.Options{})
	postChat(t, server, chatBody("any"), nil)
	entry := waitForRequests(t, handler, "a", "a-model", 1)
	if entry.Total.Successes != 1 || entry.TTFBMs.Samples != 1 || entry.TPS.Samples != 0 || entry.TPS.EWMA != nil {
		t.Fatalf("entry = %+v", entry)
	}
	if strings.Join(entry.VirtualModels, ",") != "*" {
		t.Fatalf("legacy virtual models = %v", entry.VirtualModels)
	}
}

func TestStatsClassifyServerErrorAndTimeout(t *testing.T) {
	broken := newFakeUpstream(t)
	broken.setStatus("broken-chat", http.StatusBadGateway)
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer stalled.Close()
	healthy := newFakeUpstream(t)

	cfg := testConfig(
		config.Provider{Name: "broken", BaseURL: broken.URL, APIKey: "key-broken", Priority: 3, Models: map[string]string{"chat": "broken-chat"}},
		config.Provider{Name: "stalled", BaseURL: stalled.URL, APIKey: "key-stalled", Priority: 2, Models: map[string]string{"chat": "stalled-chat"}},
		config.Provider{Name: "healthy", BaseURL: healthy.URL, APIKey: "key-healthy", Priority: 1, Models: map[string]string{"chat": "healthy-chat"}},
	)
	cfg.ResponseHeaderTimeout = 100 * time.Millisecond
	handler, server := startProxy(t, cfg, proxy.Options{})

	response := postChat(t, server, chatBody("chat"), nil)
	if response.status != http.StatusOK || response.header.Get(proxy.ProviderHeader) != "healthy" {
		t.Fatalf("status %d provider %q", response.status, response.header.Get(proxy.ProviderHeader))
	}

	brokenStats := waitForRequests(t, handler, "broken", "broken-chat", 1)
	if brokenStats.Total.Failures != 1 || brokenStats.Total.FailuresByClass.Server != 1 || brokenStats.Window.FailuresByClass.Server != 1 {
		t.Fatalf("broken = %+v", brokenStats.Total)
	}
	if brokenStats.LastErrorClass == nil || *brokenStats.LastErrorClass != "5xx" || brokenStats.LastFailureAt == nil || brokenStats.TTFBMs.Samples != 0 {
		t.Fatalf("broken last = %+v", brokenStats)
	}
	stalledStats := waitForRequests(t, handler, "stalled", "stalled-chat", 1)
	if stalledStats.Total.Failures != 1 || stalledStats.Total.FailuresByClass.Timeout != 1 {
		t.Fatalf("stalled = %+v", stalledStats.Total)
	}
	if stalledStats.LastErrorClass == nil || *stalledStats.LastErrorClass != "timeout" {
		t.Fatalf("stalled last class = %v", stalledStats.LastErrorClass)
	}
	healthyStats := waitForRequests(t, handler, "healthy", "healthy-chat", 1)
	if healthyStats.Total.Successes != 1 || healthyStats.Total.Failures != 0 {
		t.Fatalf("healthy = %+v", healthyStats.Total)
	}
}

func TestStatsClassifyConnectionErrorAndClientError(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	rejecting := newFakeUpstream(t)
	rejecting.setStatus("r-chat", http.StatusBadRequest)

	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "gone", BaseURL: closedURL, APIKey: "key-gone", Priority: 2, Models: map[string]string{"chat": "gone-chat"}},
		config.Provider{Name: "rejecting", BaseURL: rejecting.URL, APIKey: "key-r", Priority: 1, Models: map[string]string{"chat": "r-chat"}},
	), proxy.Options{})

	if response := postChat(t, server, chatBody("chat"), nil); response.status != http.StatusBadRequest {
		t.Fatalf("status %d, want the passed-through 400", response.status)
	}
	gone := waitForRequests(t, handler, "gone", "gone-chat", 1)
	if gone.Total.FailuresByClass.Connection != 1 || *gone.LastErrorClass != "connection" {
		t.Fatalf("gone = %+v", gone.Total)
	}
	rejected := waitForRequests(t, handler, "rejecting", "r-chat", 1)
	if rejected.Total.ClientErrors != 1 || rejected.Total.Failures != 0 || rejected.Total.Successes != 0 || rejected.LastFailureAt != nil {
		t.Fatalf("a passed-through 4xx is not a gate failure: %+v", rejected.Total)
	}
}

func TestStatsDoNotCountClientCancelAsFailure(t *testing.T) {
	started := make(chan struct{}, 1)
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer blocking.Close()
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "b", BaseURL: blocking.URL, APIKey: "key-b", Priority: 1, ModelAlias: "b-model"},
	), proxy.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(chatBody("x")))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	<-started
	cancel()
	<-done

	entry := waitForRequests(t, handler, "b", "b-model", 1)
	if entry.Total.Requests != 1 || entry.Total.Canceled != 1 || entry.Total.Failures != 0 || entry.LastFailureAt != nil {
		t.Fatalf("cancel = %+v", entry.Total)
	}
}

func sseUpstream(t *testing.T, firstChunkDelay time.Duration, finish bool) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		time.Sleep(firstChunkDelay)
		for index := 0; index < 3; index++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", index)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		if !finish {
			return // EOF without [DONE]
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestStatsCoverStreaming(t *testing.T) {
	const delay = 100 * time.Millisecond
	complete := sseUpstream(t, delay, true)
	cut := sseUpstream(t, 0, false)
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "s", BaseURL: complete.URL, APIKey: "key-s", Priority: 1, Models: map[string]string{"chat": "s-chat"}},
		config.Provider{Name: "cut", BaseURL: cut.URL, APIKey: "key-cut", Priority: 1, Models: map[string]string{"cut": "cut-chat"}},
	), proxy.Options{})

	response := postChat(t, server, `{"model":"chat","stream":true,"messages":[]}`, nil)
	if response.status != http.StatusOK || !strings.Contains(response.body, "[DONE]") {
		t.Fatalf("stream: %d %q", response.status, response.body)
	}
	entry := waitForRequests(t, handler, "s", "s-chat", 1)
	if entry.Total.Successes != 1 || entry.TTFBMs.Samples != 1 || *entry.TTFBMs.EWMA < float64(delay/time.Millisecond) {
		t.Fatalf("stream stats = %+v / %+v", entry.Total, entry.TTFBMs)
	}
	// 3 tokens over the ~60ms after the first chunk.
	if entry.TPS.Samples != 1 || *entry.TPS.EWMA <= 0 || *entry.TPS.EWMA > 3/(40*time.Millisecond).Seconds() {
		t.Fatalf("stream tokens/s = %+v", entry.TPS)
	}

	postChat(t, server, `{"model":"cut","stream":true,"messages":[]}`, nil)
	cutStats := waitForRequests(t, handler, "cut", "cut-chat", 1)
	if cutStats.Total.Failures != 1 || cutStats.Total.FailuresByClass.Connection != 1 || cutStats.TTFBMs.Samples != 0 {
		t.Fatalf("cut stream = %+v", cutStats)
	}
}

func TestStatusJSONShape(t *testing.T) {
	a := newFakeUpstream(t)
	const secret = "secret-key-0123456789abcdef"
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "gate-a", BaseURL: a.URL, APIKey: secret, Priority: 2, Models: map[string]string{"chat-fast": "m1", "chat-alias": "m1", "chat-smart": "m2"}},
		config.Provider{Name: "gate-b", BaseURL: a.URL, APIKey: secret + "-b", Priority: 1, ModelAlias: "legacy", Disabled: true},
	), proxy.Options{Stats: proxy.StatsSettings{Window: 5 * time.Minute, EWMAAlpha: 0.3}})
	postChat(t, server, chatBody("chat-fast"), nil)
	waitForRequests(t, handler, "gate-a", "m1", 1)

	encoded, err := json.Marshal(handler.Status())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-key") {
		t.Fatalf("status leaks an API key: %s", encoded)
	}
	var status map[string]any
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "loaded_at", "providers", "enabled_providers", "provider_status"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("existing field %q missing", key)
		}
	}
	if status["stats_window_seconds"] != 300.0 || status["stats_ewma_alpha"] != 0.3 {
		t.Fatalf("window/alpha = %v / %v", status["stats_window_seconds"], status["stats_ewma_alpha"])
	}
	stats, ok := status["stats"].([]any)
	if !ok || len(stats) != 3 {
		t.Fatalf("stats = %v", status["stats"])
	}
	var order []string
	for _, item := range stats {
		entry := item.(map[string]any)
		order = append(order, entry["provider"].(string)+"/"+entry["upstream"].(string))
		keys := make([]string, 0, len(entry))
		for key := range entry {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ","); got != "enabled,last_error_class,last_failure_at,last_success_at,provider,since,tokens_per_second,total,ttfb_ms,upstream,virtual_models,window" {
			t.Fatalf("entry keys = %s", got)
		}
		for _, block := range []string{"total", "window"} {
			counts := entry[block].(map[string]any)
			for _, key := range []string{"requests", "successes", "failures", "canceled", "client_errors"} {
				if _, ok := counts[key].(float64); !ok {
					t.Fatalf("%s.%s missing in %v", block, key, counts)
				}
			}
			classes := counts["failures_by_class"].(map[string]any)
			for _, key := range []string{"timeout", "5xx", "4xx", "connection"} {
				if _, ok := classes[key].(float64); !ok {
					t.Fatalf("%s.failures_by_class.%s missing", block, key)
				}
			}
		}
		for _, metric := range []string{"ttfb_ms", "tokens_per_second"} {
			values := entry[metric].(map[string]any)
			for _, key := range []string{"ewma", "p50", "p95", "samples", "window_samples"} {
				if _, ok := values[key]; !ok {
					t.Fatalf("%s.%s missing", metric, key)
				}
			}
		}
	}
	if got := strings.Join(order, " "); got != "gate-a/m1 gate-a/m2 gate-b/legacy" {
		t.Fatalf("stats order = %s", got)
	}
	first := stats[0].(map[string]any)
	if fmt.Sprint(first["virtual_models"]) != "[chat-alias chat-fast]" || first["enabled"] != true {
		t.Fatalf("first entry = %v", first)
	}
	if total := first["total"].(map[string]any); total["requests"] != 1.0 || total["successes"] != 1.0 {
		t.Fatalf("first total = %v", total)
	}
	if first["ttfb_ms"].(map[string]any)["ewma"] == nil {
		t.Fatal("ttfb ewma is null after a success")
	}
	second := stats[1].(map[string]any)
	if second["ttfb_ms"].(map[string]any)["ewma"] != nil || second["last_success_at"] != nil {
		t.Fatalf("unused gate must report nulls: %v", second)
	}
	if third := stats[2].(map[string]any); third["enabled"] != false {
		t.Fatalf("disabled gate = %v", third)
	}
}

func TestReloadKeepsStatsOfSurvivingGatesAndDropsRemovedOnes(t *testing.T) {
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	providerA := config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 100, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}}
	providerB := config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast"}}
	handler, server := startProxy(t, testConfig(providerA, providerB), proxy.Options{})

	postChat(t, server, chatBody("fast"), nil)
	postChat(t, server, chatBody("smart"), nil)
	waitForRequests(t, handler, "a", "a-fast", 1)
	waitForRequests(t, handler, "a", "a-smart", 1)
	since := routeStats(t, handler, "a", "a-fast").Since

	// a-smart leaves a; b changes priority; a's key rotates.
	withoutSmart := providerA
	withoutSmart.Models = map[string]string{"fast": "a-fast"}
	withoutSmart.APIKey = "key-a-rotated"
	providerB.Priority = 60
	if _, err := handler.Reload(testConfig(withoutSmart, providerB), nil); err != nil {
		t.Fatal(err)
	}
	if hasRouteStats(handler, "a", "a-smart") {
		t.Fatal("stats of a removed model survived the reload")
	}
	kept := routeStats(t, handler, "a", "a-fast")
	if kept.Total.Requests != 1 || kept.Since != since || kept.TTFBMs.Samples != 1 {
		t.Fatalf("stats of a kept gate were reset: %+v", kept)
	}
	if routeStats(t, handler, "b", "b-fast").Total.Requests != 0 {
		t.Fatal("b got traffic it never served")
	}

	// Re-adding the model starts clean.
	if _, err := handler.Reload(testConfig(providerA, providerB), nil); err != nil {
		t.Fatal(err)
	}
	if readded := routeStats(t, handler, "a", "a-smart"); readded.Total.Requests != 0 || readded.LastSuccessAt != nil {
		t.Fatalf("a re-added model inherited old stats: %+v", readded.Total)
	}

	// A removed Provider is dropped; a new base URL is a new gate.
	moved := providerA
	moved.BaseURL = b.URL
	if _, err := handler.Reload(testConfig(moved), nil); err != nil {
		t.Fatal(err)
	}
	if hasRouteStats(handler, "b", "b-fast") {
		t.Fatal("stats of a removed provider survived the reload")
	}
	if got := routeStats(t, handler, "a", "a-fast").Total.Requests; got != 0 {
		t.Fatalf("a gate with a new base_url kept %d requests", got)
	}
}
