package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
)

// lateTransport returns the upstream response only after delay, ignoring
// the request context, so the routing deadline fires between the response
// arriving and the proxy committing it.
type lateTransport struct {
	base  http.RoundTripper
	delay time.Duration
}

func (l lateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := l.base.RoundTrip(request)
	time.Sleep(l.delay)
	return response, err
}

// TestStatsDeadlineWinsCommitRace covers both commit() paths: a 200 (commit
// right after the headers) and a passed-through 400 (commit after reading the
// error body). Either way the Provider answered and the attempt is a
// deadline, not a failure, whatever set the limit.
func TestStatsDeadlineWinsCommitRace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/bad/") {
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	for _, test := range []struct{ name, base string }{{"200", upstream.URL + "/ok/v1"}, {"400", upstream.URL + "/bad/v1"}} {
		t.Run(test.name, func(t *testing.T) {
			effort := config.ReasoningEffortMax
			cfg := config.Config{
				ListenAddress:         "127.0.0.1:0",
				Cooldown:              time.Hour,
				RecoveryWait:          time.Hour,
				ResponseHeaderTimeout: 2 * time.Second,
				MaxRoutingTime:        50 * time.Millisecond, // the operator's limit
				LogLevel:              config.LogLevelError,
				ReasoningEffort:       &effort,
				Providers:             []config.Provider{{Name: "p", BaseURL: test.base, APIKey: "key-p", Priority: 1, ModelAlias: "p-model"}},
			}
			server, err := NewWithOptions(cfg, Options{Logger: log.New(io.Discard, "", 0)})
			if err != nil {
				t.Fatal(err)
			}
			server.state.Load().client = &http.Client{Transport: lateTransport{base: http.DefaultTransport, delay: 200 * time.Millisecond}}

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"x","messages":[]}`))
			server.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want the routing timeout", recorder.Code)
			}

			stats := server.Status().Stats
			if len(stats) != 1 {
				t.Fatalf("stats = %+v", stats)
			}
			total := stats[0].Total
			if total.Requests != 1 || total.Deadline != 1 || total.Failures != 0 || total.ClientErrors != 0 || stats[0].LastFailureAt != nil {
				t.Fatalf("commit race = %+v", total)
			}
		})
	}
}
