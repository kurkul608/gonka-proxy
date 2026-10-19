package proxy_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

// fakeUpstream is an httptest Provider that records what it receives and
// answers with a status chosen per upstream model.
type fakeUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []upstreamCall
	status   map[string]int // upstream model -> status; missing means 200
}

type upstreamCall struct {
	model         string
	effort        any
	hasEffort     bool
	authorization string
	routingHeader string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{status: map[string]int{}}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		effort, hasEffort := body["reasoning_effort"]
		upstream.mu.Lock()
		upstream.requests = append(upstream.requests, upstreamCall{
			model:         model,
			effort:        effort,
			hasEffort:     hasEffort,
			authorization: r.Header.Get("Authorization"),
			routingHeader: r.Header.Get(proxy.MaxRoutingTimeHeader),
		})
		status, ok := upstream.status[model]
		upstream.mu.Unlock()
		if !ok {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"model":%q}`, model)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *fakeUpstream) setStatus(model string, status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status[model] = status
}

func (u *fakeUpstream) calls() []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamCall(nil), u.requests...)
}

func (u *fakeUpstream) models() []string {
	var models []string
	for _, call := range u.calls() {
		models = append(models, call.model)
	}
	return models
}

func testConfig(providers ...config.Provider) config.Config {
	effort := config.ReasoningEffortMax
	return config.Config{
		ListenAddress:         "127.0.0.1:8080",
		Cooldown:              time.Hour,
		RecoveryWait:          time.Hour,
		ResponseHeaderTimeout: 2 * time.Second,
		LogLevel:              config.LogLevelError,
		ReasoningEffort:       &effort,
		Providers:             providers,
	}
}

func startProxy(t *testing.T, cfg config.Config, options proxy.Options) (*proxy.Server, *httptest.Server) {
	t.Helper()
	if options.Logger == nil {
		options.Logger = log.New(io.Discard, "", 0)
	}
	handler, err := proxy.NewWithOptions(cfg, options)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return handler, server
}

type proxyResponse struct {
	status int
	header http.Header
	body   string
}

func (r proxyResponse) errorCode(t *testing.T) string {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", r.body, err)
	}
	if payload.Error.Message == "" || payload.Error.Type == "" {
		t.Fatalf("error body %q lacks message or type", r.body)
	}
	return payload.Error.Code
}

func postChat(t *testing.T, server *httptest.Server, body string, headers map[string]string) proxyResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return proxyResponse{status: response.StatusCode, header: response.Header, body: string(data)}
}

func chatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[]}`, model)
}

func TestMultiModelRoutesOnlyToProvidersServingTheModel(t *testing.T) {
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	legacy := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 100, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}},
		config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast"}},
		config.Provider{Name: "legacy", BaseURL: legacy.URL, APIKey: "key-legacy", Priority: 10, ModelAlias: "legacy-model"},
	), proxy.Options{})

	response := postChat(t, server, chatBody("smart"), nil)
	if response.status != http.StatusOK {
		t.Fatalf("status = %d body %s", response.status, response.body)
	}
	if got := response.header.Get("X-Gonka-Provider"); got != "a" {
		t.Errorf("X-Gonka-Provider = %q, want a", got)
	}
	if got := response.header.Get("X-Gonka-Model"); got != "a-smart" {
		t.Errorf("X-Gonka-Model = %q, want a-smart", got)
	}

	a.setStatus("a-smart", http.StatusInternalServerError)
	response = postChat(t, server, chatBody("smart"), nil)
	if response.status != http.StatusOK {
		t.Fatalf("failover status = %d body %s", response.status, response.body)
	}
	if got := response.header.Get("X-Gonka-Provider"); got != "legacy" {
		t.Errorf("smart failover served by %q, want legacy (b does not serve smart)", got)
	}
	if got := response.header.Get("X-Gonka-Model"); got != "legacy-model" {
		t.Errorf("X-Gonka-Model = %q, want legacy-model", got)
	}
	if calls := b.calls(); len(calls) != 0 {
		t.Errorf("b received %d requests for a model it does not serve", len(calls))
	}
	if got, want := a.models(), []string{"a-smart", "a-smart"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a upstream models = %v, want %v", got, want)
	}
}

func TestUnknownModelReturnsModelNotFound(t *testing.T) {
	a := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 1, Models: map[string]string{"fast": "a-fast"}},
	), proxy.Options{})

	for name, body := range map[string]string{
		"unknown model": chatBody("nope"),
		"missing model": `{"messages":[]}`,
		"non-string":    `{"model":42,"messages":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := postChat(t, server, body, nil)
			if response.status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %s)", response.status, response.body)
			}
			if code := response.errorCode(t); code != proxy.CodeModelNotFound {
				t.Fatalf("code = %q, want %q", code, proxy.CodeModelNotFound)
			}
			if !strings.Contains(response.header.Get("Content-Type"), "application/json") {
				t.Errorf("Content-Type = %q", response.header.Get("Content-Type"))
			}
		})
	}
	if calls := a.calls(); len(calls) != 0 {
		t.Fatalf("upstream received %d requests", len(calls))
	}
	// The 404 never touches cooldowns: the known model still routes.
	if response := postChat(t, server, chatBody("fast"), nil); response.status != http.StatusOK {
		t.Fatalf("known model status = %d", response.status)
	}
}

func TestLegacyProvidersServeAnyModel(t *testing.T) {
	legacy := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "legacy", BaseURL: legacy.URL, APIKey: "key-legacy", Priority: 1, ModelAlias: "legacy-model"},
	), proxy.Options{})
	for _, body := range []string{chatBody("anything"), `{"messages":[]}`} {
		if response := postChat(t, server, body, nil); response.status != http.StatusOK {
			t.Fatalf("status = %d for %s", response.status, body)
		}
	}
	if got, want := legacy.models(), []string{"legacy-model", "legacy-model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream models = %v, want %v", got, want)
	}
}

func TestCooldownScope(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		failoverOn404  bool
		failoverOnAuth bool
		providerWide   bool
	}{
		{name: "500 cools only the model", status: http.StatusInternalServerError},
		{name: "503 cools only the model", status: http.StatusServiceUnavailable},
		{name: "404 cools only the model", status: http.StatusNotFound, failoverOn404: true},
		{name: "429 cools the provider", status: http.StatusTooManyRequests, providerWide: true},
		{name: "402 cools the provider", status: http.StatusPaymentRequired, providerWide: true},
		{name: "401 cools the provider when enabled", status: http.StatusUnauthorized, failoverOnAuth: true, providerWide: true},
		{name: "403 cools the provider when enabled", status: http.StatusForbidden, failoverOnAuth: true, providerWide: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := newFakeUpstream(t)
			b := newFakeUpstream(t)
			cfg := testConfig(
				config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 100, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}},
				config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast", "smart": "b-smart"}},
			)
			cfg.FailoverOn404 = test.failoverOn404
			cfg.FailoverOnAuthError = test.failoverOnAuth
			handler, server := startProxy(t, cfg, proxy.Options{})

			a.setStatus("a-smart", test.status)
			response := postChat(t, server, chatBody("smart"), nil)
			if response.status != http.StatusOK || response.header.Get("X-Gonka-Provider") != "b" {
				t.Fatalf("smart: status %d provider %q, want b to serve after failover", response.status, response.header.Get("X-Gonka-Provider"))
			}

			response = postChat(t, server, chatBody("fast"), nil)
			wantProvider := "a"
			if test.providerWide {
				wantProvider = "b"
			}
			if got := response.header.Get("X-Gonka-Provider"); got != wantProvider {
				t.Fatalf("fast served by %q, want %q", got, wantProvider)
			}

			status := handler.Status()
			providerA := status.ProviderStatuses[0]
			if providerA.Name != "a" {
				t.Fatalf("status order = %+v", status.ProviderStatuses)
			}
			var smart, fast proxy.ModelStatus
			for _, model := range providerA.Models {
				switch model.Virtual {
				case "smart":
					smart = model
				case "fast":
					fast = model
				}
			}
			if smart.Available || smart.CooldownUntil == nil || smart.LastFailure == nil {
				t.Fatalf("smart status = %+v, want cooling with a recorded failure", smart)
			}
			if smart.LastFailure.Kind != "status" || smart.LastFailure.Status != test.status {
				t.Errorf("last failure = %+v, want status %d", smart.LastFailure, test.status)
			}
			wantScope := "model"
			if test.providerWide {
				wantScope = "provider"
			}
			if smart.LastFailure.Scope != wantScope {
				t.Errorf("scope = %q, want %q", smart.LastFailure.Scope, wantScope)
			}
			if fast.Available == test.providerWide {
				t.Errorf("fast available = %t, want %t", fast.Available, !test.providerWide)
			}
			if providerA.Available == test.providerWide {
				t.Errorf("provider available = %t, want %t", providerA.Available, !test.providerWide)
			}
			if _, err := time.Parse(time.RFC3339, *smart.CooldownUntil); err != nil {
				t.Errorf("cooldown_until %q is not RFC3339: %v", *smart.CooldownUntil, err)
			}
		})
	}
}

func TestNetworkFailureCoolsTheProvider(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	b := newFakeUpstream(t)
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: downURL, APIKey: "key-a", Priority: 100, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}},
		config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast", "smart": "b-smart"}},
	), proxy.Options{})
	if response := postChat(t, server, chatBody("smart"), nil); response.header.Get("X-Gonka-Provider") != "b" {
		t.Fatalf("status %d provider %q", response.status, response.header.Get("X-Gonka-Provider"))
	}
	providerA := handler.Status().ProviderStatuses[0]
	if providerA.Available || providerA.LastFailure == nil || providerA.LastFailure.Kind != "network" {
		t.Fatalf("provider a status = %+v, want provider-wide network cooldown", providerA)
	}
}

func TestUnsupportedReasoningEffortCoolsOnlyTheModel(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "a-smart") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"reasoning_effort: unsupported value: max"}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer a.Close()
	b := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 100, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}},
		config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast", "smart": "b-smart"}},
	), proxy.Options{})
	if got := postChat(t, server, chatBody("smart"), nil).header.Get("X-Gonka-Provider"); got != "b" {
		t.Fatalf("smart served by %q, want b", got)
	}
	if got := postChat(t, server, chatBody("fast"), nil).header.Get("X-Gonka-Provider"); got != "a" {
		t.Fatalf("fast served by %q, want a", got)
	}
}

func TestReasoningEffortPrecedence(t *testing.T) {
	boolPtr := func(value bool) *bool { return &value }
	tests := []struct {
		name          string
		allowClient   bool
		model         *config.ModelSettings
		provider      config.Provider
		requestEffort string // raw JSON value, "" for absent
		wantStatus    int
		wantEffort    any // nil means absent upstream
	}{
		{name: "global value", wantEffort: "max"},
		{name: "model default beats global", model: &config.ModelSettings{ReasoningEffort: effortPtr("low")}, wantEffort: "low"},
		{name: "model strip beats global", model: &config.ModelSettings{StripReasoningEffort: true}, wantEffort: nil},
		{name: "client ignored by default", requestEffort: `"none"`, model: &config.ModelSettings{ReasoningEffort: effortPtr("low")}, wantEffort: "low"},
		{name: "allowed client beats model default", allowClient: true, requestEffort: `"High"`, model: &config.ModelSettings{ReasoningEffort: effortPtr("low")}, wantEffort: "high"},
		{name: "allowed client beats model strip", allowClient: true, requestEffort: `"medium"`, model: &config.ModelSettings{StripReasoningEffort: true}, wantEffort: "medium"},
		{name: "allowed client null falls back to model", allowClient: true, requestEffort: `null`, model: &config.ModelSettings{ReasoningEffort: effortPtr("low")}, wantEffort: "low"},
		{name: "model allows client when global does not", requestEffort: `"xhigh"`, model: &config.ModelSettings{AllowClientReasoningEffort: boolPtr(true)}, wantEffort: "xhigh"},
		{name: "model forbids client when global allows", allowClient: true, requestEffort: `"xhigh"`, model: &config.ModelSettings{AllowClientReasoningEffort: boolPtr(false)}, wantEffort: "max"},
		{name: "provider override beats client", allowClient: true, requestEffort: `"low"`, provider: config.Provider{ReasoningEffort: effortPtr("high")}, wantEffort: "high"},
		{name: "provider strip beats client", allowClient: true, requestEffort: `"low"`, provider: config.Provider{StripReasoningEffort: true}, wantEffort: nil},
		{name: "provider override beats model default", provider: config.Provider{ReasoningEffort: effortPtr("medium")}, model: &config.ModelSettings{ReasoningEffort: effortPtr("low")}, wantEffort: "medium"},
		{name: "invalid allowed client value is a 400", allowClient: true, requestEffort: `"extreme"`, wantStatus: http.StatusBadRequest},
		{name: "non-string allowed client value is a 400", allowClient: true, requestEffort: `3`, wantStatus: http.StatusBadRequest},
		{name: "invalid client value ignored when not allowed", requestEffort: `"extreme"`, wantEffort: "max"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newFakeUpstream(t)
			provider := test.provider
			provider.Name, provider.BaseURL, provider.APIKey, provider.Priority = "p", upstream.URL, "key-p", 1
			provider.Models = map[string]string{"chat": "real-chat"}
			cfg := testConfig(provider)
			cfg.AllowClientReasoningEffort = test.allowClient
			if test.model != nil {
				cfg.Models = map[string]config.ModelSettings{"chat": *test.model}
			}
			_, server := startProxy(t, cfg, proxy.Options{})

			body := `{"model":"chat","messages":[]}`
			if test.requestEffort != "" {
				body = `{"model":"chat","messages":[],"reasoning_effort":` + test.requestEffort + `}`
			}
			response := postChat(t, server, body, nil)
			wantStatus := test.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			if response.status != wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", response.status, wantStatus, response.body)
			}
			calls := upstream.calls()
			if wantStatus != http.StatusOK {
				if code := response.errorCode(t); code != proxy.CodeInvalidReasoningEffort {
					t.Errorf("code = %q, want %q", code, proxy.CodeInvalidReasoningEffort)
				}
				if len(calls) != 0 {
					t.Errorf("upstream called %d times for a rejected request", len(calls))
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("upstream calls = %d, want 1", len(calls))
			}
			if test.wantEffort == nil {
				if calls[0].hasEffort {
					t.Fatalf("upstream reasoning_effort = %v, want absent", calls[0].effort)
				}
				return
			}
			if calls[0].effort != test.wantEffort {
				t.Fatalf("upstream reasoning_effort = %v, want %v", calls[0].effort, test.wantEffort)
			}
		})
	}
}

func TestZeroProviders(t *testing.T) {
	cfg := testConfig()
	_, server := startProxy(t, cfg, proxy.Options{})

	started := time.Now()
	response := postChat(t, server, chatBody("anything"), nil)
	if response.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.status)
	}
	if code := response.errorCode(t); code != proxy.CodeNoProviderAvailable {
		t.Fatalf("code = %q", code)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("zero providers took %s, want an immediate answer (no Recovery Wait)", elapsed)
	}
	if ids := listModels(t, server, nil); len(ids) != 0 {
		t.Fatalf("models = %v, want empty", ids)
	}
}

func TestDisabledProviders(t *testing.T) {
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 100, Disabled: true, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}},
		config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast"}},
	), proxy.Options{})

	if got := postChat(t, server, chatBody("fast"), nil).header.Get("X-Gonka-Provider"); got != "b" {
		t.Fatalf("fast served by %q, want b (a is disabled)", got)
	}
	started := time.Now()
	response := postChat(t, server, chatBody("smart"), nil)
	if response.status != http.StatusServiceUnavailable || response.errorCode(t) != proxy.CodeNoProviderAvailable {
		t.Fatalf("smart (only on a disabled provider): status %d body %s", response.status, response.body)
	}
	if time.Since(started) > time.Second {
		t.Fatal("a model served only by disabled providers must fail immediately")
	}
	if len(a.calls()) != 0 {
		t.Fatal("disabled provider received requests")
	}
	status := handler.Status()
	if status.Providers != 2 || status.EnabledProviders != 1 || status.ProviderStatuses[0].Enabled || status.ProviderStatuses[0].Available {
		t.Fatalf("status = %+v, want a reported as disabled", status)
	}
}

func listModels(t *testing.T, server *httptest.Server, headers map[string]string) []string {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d", response.StatusCode)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || payload.Data == nil {
		t.Fatalf("models payload = %+v, want an object=list with a data array", payload)
	}
	ids := []string{}
	for _, model := range payload.Data {
		if model.Object != "model" {
			t.Fatalf("model object = %q", model.Object)
		}
		ids = append(ids, model.ID)
	}
	return ids
}

func TestModelsListsVirtualModels(t *testing.T) {
	cfg := testConfig(
		config.Provider{Name: "a", BaseURL: "https://a.example/v1", APIKey: "key-a", Priority: 1, Models: map[string]string{"zeta": "a-z", "alpha": "a-a"}},
		config.Provider{Name: "legacy", BaseURL: "https://l.example/v1", APIKey: "key-l", Priority: 5, ModelAlias: "legacy-model"},
	)
	cfg.Models = map[string]config.ModelSettings{"mid": {}}
	_, server := startProxy(t, cfg, proxy.Options{})
	if got, want := listModels(t, server, nil), []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}

	response, err := http.Post(server.URL+"/v1/models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models status = %d", response.StatusCode)
	}
}

func TestModelsListsUniqueAliasesInPriorityOrder(t *testing.T) {
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "low", BaseURL: "https://low.example/v1", APIKey: "low-secret", ModelAlias: "model-b", Priority: 10},
		config.Provider{Name: "high", BaseURL: "https://high.example/v1", APIKey: "high-secret", ModelAlias: "model-a", Priority: 100},
		config.Provider{Name: "mid", BaseURL: "https://mid.example/v1", APIKey: "mid-secret", ModelAlias: "model-a", Priority: 50},
	), proxy.Options{})
	if got, want := listModels(t, server, nil), []string{"model-a", "model-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

func TestClientToken(t *testing.T) {
	upstream := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "provider-key", Priority: 1, ModelAlias: "real"},
	), proxy.Options{ClientToken: "client-token-0123456789"})

	for name, authorization := range map[string]string{
		"missing":       "",
		"wrong":         "Bearer client-token-wrong",
		"prefix only":   "Bearer client-token",
		"wrong scheme":  "Basic client-token-0123456789",
		"provider key":  "Bearer provider-key",
		"empty bearer":  "Bearer ",
		"token no auth": "client-token-0123456789",
	} {
		t.Run(name, func(t *testing.T) {
			headers := map[string]string{}
			if authorization != "" {
				headers["Authorization"] = authorization
			}
			response := postChat(t, server, chatBody("v"), headers)
			if response.status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.status)
			}
			if code := response.errorCode(t); code != proxy.CodeInvalidAPIKey {
				t.Fatalf("code = %q", code)
			}
		})
	}

	response := postChat(t, server, chatBody("v"), map[string]string{"Authorization": "Bearer client-token-0123456789"})
	if response.status != http.StatusOK {
		t.Fatalf("authorized status = %d", response.status)
	}
	calls := upstream.calls()
	if len(calls) != 1 || calls[0].authorization != "Bearer provider-key" {
		t.Fatalf("upstream calls = %+v, want one call with the provider key", calls)
	}

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	unauthorized, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /v1/models without token = %d, want 401", unauthorized.StatusCode)
	}
	if ids := listModels(t, server, map[string]string{"Authorization": "bearer client-token-0123456789"}); len(ids) != 1 {
		t.Fatalf("models = %v", ids)
	}
}

func TestMaxRoutingTime(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()

	tests := []struct {
		name       string
		configured time.Duration
		header     string
		wantStatus int
		maxElapsed time.Duration
		minElapsed time.Duration
	}{
		{name: "config limit", configured: 150 * time.Millisecond, wantStatus: http.StatusServiceUnavailable, minElapsed: 100 * time.Millisecond, maxElapsed: 2 * time.Second},
		{name: "header limit without config", header: "150ms", wantStatus: http.StatusServiceUnavailable, minElapsed: 100 * time.Millisecond, maxElapsed: 2 * time.Second},
		{name: "header in seconds", header: "0.15", wantStatus: http.StatusServiceUnavailable, minElapsed: 100 * time.Millisecond, maxElapsed: 2 * time.Second},
		{name: "header capped by config", configured: 150 * time.Millisecond, header: "1h", wantStatus: http.StatusServiceUnavailable, minElapsed: 100 * time.Millisecond, maxElapsed: 2 * time.Second},
		{name: "header shortens config", configured: time.Hour, header: "150ms", wantStatus: http.StatusServiceUnavailable, minElapsed: 100 * time.Millisecond, maxElapsed: 2 * time.Second},
		{name: "invalid header", header: "soon", wantStatus: http.StatusBadRequest, maxElapsed: time.Second},
		{name: "zero header", header: "0s", wantStatus: http.StatusBadRequest, maxElapsed: time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(config.Provider{Name: "p", BaseURL: failing.URL, APIKey: "key-p", Priority: 1, ModelAlias: "real"})
			cfg.MaxRoutingTime = test.configured
			cfg.RecoveryWait = 20 * time.Millisecond
			_, server := startProxy(t, cfg, proxy.Options{})
			headers := map[string]string{}
			if test.header != "" {
				headers[proxy.MaxRoutingTimeHeader] = test.header
			}
			started := time.Now()
			response := postChat(t, server, chatBody("v"), headers)
			elapsed := time.Since(started)
			if response.status != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", response.status, test.wantStatus, response.body)
			}
			wantCode := proxy.CodeNoProviderAvailable
			if test.wantStatus == http.StatusBadRequest {
				wantCode = proxy.CodeInvalidMaxRoutingTime
			}
			if code := response.errorCode(t); code != wantCode {
				t.Fatalf("code = %q, want %q", code, wantCode)
			}
			if elapsed < test.minElapsed || elapsed > test.maxElapsed {
				t.Fatalf("elapsed = %s, want between %s and %s", elapsed, test.minElapsed, test.maxElapsed)
			}
		})
	}
}

func TestMaxRoutingTimeAbortsSlowAttemptWithoutCooldown(t *testing.T) {
	var canceled atomic.Bool
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // lets the server notice the client hanging up
		select {
		case <-r.Context().Done():
			canceled.Store(true)
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	cfg := testConfig(config.Provider{Name: "p", BaseURL: slow.URL, APIKey: "key-p", Priority: 1, ModelAlias: "real"})
	cfg.ResponseHeaderTimeout = 10 * time.Second
	handler, server := startProxy(t, cfg, proxy.Options{})

	started := time.Now()
	response := postChat(t, server, chatBody("v"), map[string]string{proxy.MaxRoutingTimeHeader: "100ms"})
	if response.status != http.StatusServiceUnavailable || time.Since(started) > 2*time.Second {
		t.Fatalf("status %d after %s, want a quick 503", response.status, time.Since(started))
	}
	deadline := time.Now().Add(time.Second)
	for !canceled.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !canceled.Load() {
		t.Fatal("the in-flight upstream attempt was not canceled")
	}
	if status := handler.Status(); !status.ProviderStatuses[0].Models[0].Available {
		t.Fatalf("routing timeout cooled the provider: %+v", status.ProviderStatuses[0])
	}
}

func TestMaxRoutingTimeDoesNotCutStreams(t *testing.T) {
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for index := 0; index < 5; index++ {
			_, _ = fmt.Fprintf(w, "data: {\"chunk\":%d}\n\n", index)
			flusher.Flush()
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer streaming.Close()
	cfg := testConfig(config.Provider{Name: "p", BaseURL: streaming.URL, APIKey: "key-p", Priority: 1, ModelAlias: "real"})
	cfg.MaxRoutingTime = 100 * time.Millisecond
	_, server := startProxy(t, cfg, proxy.Options{})

	response := postChat(t, server, `{"model":"v","messages":[],"stream":true}`, nil)
	if response.status != http.StatusOK {
		t.Fatalf("status = %d", response.status)
	}
	if !strings.Contains(response.body, `{"chunk":4}`) || !strings.HasSuffix(response.body, "data: [DONE]\n\n") {
		t.Fatalf("stream was cut: %q", response.body)
	}
}

func TestMaxRoutingTimeHeaderNotForwarded(t *testing.T) {
	upstream := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(config.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "key-p", Priority: 1, ModelAlias: "real"}), proxy.Options{})
	if response := postChat(t, server, chatBody("v"), map[string]string{proxy.MaxRoutingTimeHeader: "5s"}); response.status != http.StatusOK {
		t.Fatalf("status = %d", response.status)
	}
	if calls := upstream.calls(); len(calls) != 1 || calls[0].routingHeader != "" {
		t.Fatalf("upstream saw %q, want the routing header stripped", calls[0].routingHeader)
	}
}

func TestOversizedChatBodyIsRejected(t *testing.T) {
	a := newFakeUpstream(t)
	_, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 1, Models: map[string]string{"fast": "a-fast"}},
	), proxy.Options{})

	padding := strings.Repeat("x", proxy.MaxChatBodyBytes)
	response := postChat(t, server, `{"model":"fast","messages":[{"role":"user","content":"`+padding+`"}]}`, nil)
	if response.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.status)
	}
	if code := response.errorCode(t); code != proxy.CodeRequestTooLarge {
		t.Fatalf("code = %q, want %q", code, proxy.CodeRequestTooLarge)
	}
	if calls := a.calls(); len(calls) != 0 {
		t.Fatalf("upstream received %d requests", len(calls))
	}
	if response := postChat(t, server, chatBody("fast"), nil); response.status != http.StatusOK {
		t.Fatalf("normal request status = %d", response.status)
	}
}
