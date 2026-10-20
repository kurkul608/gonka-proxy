package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

func TestStatsCountPassedThroughAuthAndNotFoundAsFailures(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.setStatus("m401", http.StatusUnauthorized)
	upstream.setStatus("m403", http.StatusForbidden)
	upstream.setStatus("m404", http.StatusNotFound)
	upstream.setStatus("m422", http.StatusUnprocessableEntity)
	// The failover switches stay off: these answers go back to the client.
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "key-p", Priority: 1, Models: map[string]string{
			"v401": "m401", "v403": "m403", "v404": "m404", "v422": "m422",
		}},
	), proxy.Options{})

	for _, status := range []int{401, 403, 404} {
		virtual, upstreamModel := fmt.Sprintf("v%d", status), fmt.Sprintf("m%d", status)
		if response := postChat(t, server, chatBody(virtual), nil); response.status != status {
			t.Fatalf("%s: status %d, want the passed-through %d", virtual, response.status, status)
		}
		entry := waitForRequests(t, handler, "p", upstreamModel, 1)
		if entry.Total.Failures != 1 || entry.Total.FailuresByClass.Client != 1 || entry.Total.ClientErrors != 0 {
			t.Fatalf("%d must be a 4xx failure: %+v", status, entry.Total)
		}
		if entry.LastErrorClass == nil || *entry.LastErrorClass != "4xx" || entry.LastFailureAt == nil {
			t.Fatalf("%d last failure = %v %v", status, entry.LastErrorClass, entry.LastFailureAt)
		}
	}

	if response := postChat(t, server, chatBody("v422"), nil); response.status != http.StatusUnprocessableEntity {
		t.Fatalf("422: status %d", response.status)
	}
	entry := waitForRequests(t, handler, "p", "m422", 1)
	if entry.Total.ClientErrors != 1 || entry.Total.Failures != 0 {
		t.Fatalf("422 must be a client error: %+v", entry.Total)
	}
}

func TestStatsClientRoutingDeadlineIsNotAProviderTimeout(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer stalled.Close()
	provider := config.Provider{Name: "s", BaseURL: stalled.URL, APIKey: "key-s", Priority: 1, ModelAlias: "s-model"}

	t.Run("X-Gonka-Max-Routing-Time", func(t *testing.T) {
		handler, server := startProxy(t, testConfig(provider), proxy.Options{})
		response := postChat(t, server, chatBody("x"), map[string]string{proxy.MaxRoutingTimeHeader: "100ms"})
		if response.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d", response.status)
		}
		entry := waitForRequests(t, handler, "s", "s-model", 1)
		if entry.Total.Deadline != 1 || entry.Window.Deadline != 1 || entry.Total.Failures != 0 || entry.LastFailureAt != nil {
			t.Fatalf("client deadline = %+v", entry.Total)
		}
	})

	t.Run("max_routing_time", func(t *testing.T) {
		cfg := testConfig(provider)
		cfg.MaxRoutingTime = 100 * time.Millisecond
		handler, server := startProxy(t, cfg, proxy.Options{})
		if response := postChat(t, server, chatBody("x"), nil); response.status != http.StatusServiceUnavailable {
			t.Fatalf("status %d", response.status)
		}
		entry := waitForRequests(t, handler, "s", "s-model", 1)
		if entry.Total.Deadline != 0 || entry.Total.Failures != 1 || entry.Total.FailuresByClass.Timeout != 1 {
			t.Fatalf("operator routing limit = %+v, want a Provider timeout", entry.Total)
		}
	})

	t.Run("header equal to max_routing_time", func(t *testing.T) {
		cfg := testConfig(provider)
		cfg.MaxRoutingTime = 100 * time.Millisecond
		handler, server := startProxy(t, cfg, proxy.Options{})
		postChat(t, server, chatBody("x"), map[string]string{proxy.MaxRoutingTimeHeader: "1s"}) // capped to 100ms
		entry := waitForRequests(t, handler, "s", "s-model", 1)
		if entry.Total.FailuresByClass.Timeout != 1 || entry.Total.Deadline != 0 {
			t.Fatalf("a header capped to max_routing_time = %+v, want a Provider timeout", entry.Total)
		}
	})
}

// dualUpstream answers a stream with SSE and anything else with JSON, both
// with usage.
func dualUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, completionWithUsage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for index := 0; index < 3; index++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", index)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestStatsSplitStreamAndNonStreamThroughHandler(t *testing.T) {
	upstream := dualUpstream(t)
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "d", BaseURL: upstream.URL, APIKey: "key-d", Priority: 1, ModelAlias: "d-model"},
	), proxy.Options{})

	postChat(t, server, `{"model":"x","messages":[]}`, nil)
	postChat(t, server, `{"model":"x","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, nil)
	postChat(t, server, `{"model":"x","stream":true,"messages":[]}`, nil)
	entry := waitForRequests(t, handler, "d", "d-model", 3)
	if entry.Total.Successes != 3 {
		t.Fatalf("counts = %+v", entry.Total)
	}
	if entry.TTFBMs.NonStream.Samples != 1 || entry.TTFBMs.Stream.Samples != 2 {
		t.Fatalf("ttfb split = %+v", entry.TTFBMs)
	}
	if entry.TPS.NonStream.Samples != 1 || entry.TPS.Stream.Samples != 2 {
		t.Fatalf("tokens/s split = %+v", entry.TPS)
	}
}

func TestStatsClassifyConnectionResetMidStream(t *testing.T) {
	resetting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
		_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = fmt.Fprintf(buffer, "%x\r\n%s\r\n", len(chunk), chunk)
		_ = buffer.Flush()
		time.Sleep(20 * time.Millisecond)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) // close with RST
		}
		_ = conn.Close()
	}))
	defer resetting.Close()
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "r", BaseURL: resetting.URL, APIKey: "key-r", Priority: 1, ModelAlias: "r-model"},
	), proxy.Options{})

	response := postChat(t, server, `{"model":"x","stream":true,"messages":[]}`, nil)
	if response.status != http.StatusOK {
		t.Fatalf("status %d", response.status)
	}
	entry := waitForRequests(t, handler, "r", "r-model", 1)
	if entry.Total.Failures != 1 || entry.Total.FailuresByClass.Connection != 1 || entry.Total.Canceled != 0 {
		t.Fatalf("reset mid-stream = %+v", entry.Total)
	}
	if entry.TTFBMs.Stream.Samples != 0 || *entry.LastErrorClass != "connection" {
		t.Fatalf("reset mid-stream entry = %+v", entry)
	}
}

// heldUpstream holds each request until release is closed, then answers
// chunked (no Content-Length), so the client sees the end of the response
// only after the proxy handler, and its stats record, are done.
func heldUpstream(t *testing.T) (*httptest.Server, chan struct{}, chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, completionWithUsage)
	}))
	t.Cleanup(upstream.Close)
	return upstream, started, release
}

func TestStatsInFlightRequestAfterItsRouteWasRemoved(t *testing.T) {
	for _, readd := range []bool{false, true} {
		t.Run(fmt.Sprintf("readd=%v", readd), func(t *testing.T) {
			upstream, started, release := heldUpstream(t)
			full := config.Provider{Name: "a", BaseURL: upstream.URL, APIKey: "key-a", Priority: 1, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}}
			withoutSmart := full
			withoutSmart.Models = map[string]string{"fast": "a-fast"}
			handler, server := startProxy(t, testConfig(full), proxy.Options{})

			result := make(chan proxyResponse, 1)
			go func() { result <- postChat(t, server, chatBody("smart"), nil) }()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("request never reached the upstream")
			}
			if _, err := handler.Reload(testConfig(withoutSmart), nil); err != nil {
				t.Fatal(err)
			}
			if readd {
				if _, err := handler.Reload(testConfig(full), nil); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if response := <-result; response.status != http.StatusOK || response.header.Get(proxy.ModelHeader) != "a-smart" {
				t.Fatalf("in-flight request: %d %q", response.status, response.header.Get(proxy.ModelHeader))
			}

			if !readd {
				if hasRouteStats(handler, "a", "a-smart") {
					t.Fatal("an in-flight request brought a removed route back")
				}
			} else if entry := routeStats(t, handler, "a", "a-smart"); entry.Total.Requests != 0 || entry.LastSuccessAt != nil {
				t.Fatalf("the re-added route got the old snapshot's attempt: %+v", entry.Total)
			}
			if entry := routeStats(t, handler, "a", "a-fast"); entry.Total.Requests != 0 {
				t.Fatalf("a-fast = %+v", entry.Total)
			}

			// New requests on the current snapshot are counted.
			go func() { result <- postChat(t, server, chatBody("fast"), nil) }()
			<-started
			<-result
			if entry := routeStats(t, handler, "a", "a-fast"); entry.Total.Successes != 1 {
				t.Fatalf("a-fast after a new request = %+v", entry.Total)
			}
		})
	}
}

func TestStatusConcurrentWithTraffic(t *testing.T) {
	upstream := dualUpstream(t)
	broken := newFakeUpstream(t)
	broken.setStatus("b-model", http.StatusInternalServerError)
	cfg := testConfig(
		config.Provider{Name: "broken", BaseURL: broken.URL, APIKey: "key-b", Priority: 2, ModelAlias: "b-model"},
		config.Provider{Name: "d", BaseURL: upstream.URL, APIKey: "key-d", Priority: 1, ModelAlias: "d-model"},
	)
	cfg.Cooldown = time.Millisecond // keep failing over to broken
	handler, server := startProxy(t, cfg, proxy.Options{})

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for reader := 0; reader < 2; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := json.Marshal(handler.Status()); err != nil {
					t.Errorf("marshal status: %v", err)
					return
				}
			}
		}()
	}
	var clients sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		clients.Add(1)
		go func(worker int) {
			defer clients.Done()
			for request := 0; request < 15; request++ {
				body := `{"model":"x","messages":[]}`
				if (worker+request)%2 == 0 {
					body = `{"model":"x","stream":true,"messages":[]}`
				}
				if response := postChat(t, server, body, nil); response.status != http.StatusOK {
					t.Errorf("status %d", response.status)
					return
				}
			}
		}(worker)
	}
	clients.Wait()
	close(stop)
	readers.Wait()

	entry := waitForRequests(t, handler, "d", "d-model", 90)
	if entry.Total.Successes != 90 || entry.TTFBMs.Stream.Samples+entry.TTFBMs.NonStream.Samples != 90 {
		t.Fatalf("d = %+v / %+v", entry.Total, entry.TTFBMs)
	}
}
