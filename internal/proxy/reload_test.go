package proxy_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

func TestReloadKeepsInFlightRequestsOnTheirSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	slowA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(w, `{"from":"a"}`)
	}))
	defer slowA.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	b := newFakeUpstream(t)

	handler, server := startProxy(t, testConfig(
		config.Provider{Name: "a", BaseURL: slowA.URL, APIKey: "key-a", Priority: 1, ModelAlias: "a-model"},
	), proxy.Options{})

	firstResult := make(chan proxyResponse, 1)
	go func() {
		firstResult <- postChat(t, server, chatBody("v"), nil)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached provider a")
	}

	result, err := handler.Reload(testConfig(
		config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 1, ModelAlias: "b-model"},
	), nil)
	if err != nil || !result.Changed {
		t.Fatalf("Reload = %+v, %v", result, err)
	}

	second := postChat(t, server, chatBody("v"), nil)
	if second.status != http.StatusOK || second.header.Get("X-Gonka-Provider") != "b" {
		t.Fatalf("request after reload: status %d provider %q, want b", second.status, second.header.Get("X-Gonka-Provider"))
	}

	close(release)
	select {
	case first := <-firstResult:
		if first.status != http.StatusOK || first.header.Get("X-Gonka-Provider") != "a" || first.header.Get("X-Gonka-Model") != "a-model" {
			t.Fatalf("in-flight request: status %d provider %q model %q, want a/a-model", first.status, first.header.Get("X-Gonka-Provider"), first.header.Get("X-Gonka-Model"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish")
	}
}

func TestReloadUnderConcurrentLoad(t *testing.T) {
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	configA := testConfig(config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 1, Models: map[string]string{"chat": "a-chat"}})
	configB := testConfig(
		config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 2, Models: map[string]string{"chat": "b-chat"}},
		config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 1, ModelAlias: "a-legacy"},
	)
	configB.LogLevel = config.LogLevelInfo
	configB.ResponseHeaderTimeout = 3 * time.Second // forces a new transport on every other reload
	handler, server := startProxy(t, configA, proxy.Options{})

	stop := make(chan struct{})
	var reloads sync.WaitGroup
	reloads.Add(1)
	go func() {
		defer reloads.Done()
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
			}
			next := configA
			if index%2 == 0 {
				next = configB
			}
			if _, err := handler.Reload(next, nil); err != nil {
				t.Errorf("Reload: %v", err)
				return
			}
			_ = handler.Status()
			time.Sleep(time.Millisecond)
		}
	}()

	var clients sync.WaitGroup
	failures := make(chan string, 200)
	for worker := 0; worker < 8; worker++ {
		clients.Add(1)
		go func() {
			defer clients.Done()
			for request := 0; request < 25; request++ {
				response := postChat(t, server, chatBody("chat"), nil)
				if response.status != http.StatusOK {
					failures <- response.body
					return
				}
				switch model := response.header.Get("X-Gonka-Model"); model {
				case "a-chat", "b-chat":
				default:
					failures <- "unexpected model " + model
					return
				}
			}
		}()
	}
	clients.Wait()
	close(stop)
	reloads.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("request failed during reloads: %s", failure)
	}
}

func TestReloadPreservesCooldownsOfUnchangedProviders(t *testing.T) {
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	providerA := config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 100, Models: map[string]string{"fast": "a-fast", "smart": "a-smart"}}
	providerB := config.Provider{Name: "b", BaseURL: b.URL, APIKey: "key-b", Priority: 50, Models: map[string]string{"fast": "b-fast", "smart": "b-smart"}}
	handler, server := startProxy(t, testConfig(providerA, providerB), proxy.Options{})

	a.setStatus("a-smart", http.StatusInternalServerError)
	postChat(t, server, chatBody("smart"), nil)
	a.setStatus("a-smart", http.StatusOK)

	smartAvailable := func() bool {
		for _, provider := range handler.Status().ProviderStatuses {
			if provider.Name != "a" {
				continue
			}
			for _, model := range provider.Models {
				if model.Virtual == "smart" {
					return model.Available
				}
			}
		}
		t.Fatal("provider a / smart not in status")
		return false
	}
	if smartAvailable() {
		t.Fatal("a/smart should be cooling after a 500")
	}

	// Same name, URL and key: the cooldown survives an unrelated change.
	providerB.Priority = 60
	if _, err := handler.Reload(testConfig(providerA, providerB), nil); err != nil {
		t.Fatal(err)
	}
	if smartAvailable() {
		t.Fatal("reload dropped the cooldown of an unchanged provider")
	}
	if got := postChat(t, server, chatBody("smart"), nil).header.Get("X-Gonka-Provider"); got != "b" {
		t.Fatalf("smart served by %q, want b while a/smart cools", got)
	}

	// The model leaves provider a: its cooldown is dropped with it.
	withoutSmart := providerA
	withoutSmart.Models = map[string]string{"fast": "a-fast"}
	if _, err := handler.Reload(testConfig(withoutSmart, providerB), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Reload(testConfig(providerA, providerB), nil); err != nil {
		t.Fatal(err)
	}
	if !smartAvailable() {
		t.Fatal("a removed model's cooldown came back after it was re-added")
	}

	// A rotated key is a different provider: no inherited cooldown.
	a.setStatus("a-smart", http.StatusInternalServerError)
	postChat(t, server, chatBody("smart"), nil)
	a.setStatus("a-smart", http.StatusOK)
	rotated := providerA
	rotated.APIKey = "key-a-rotated"
	if _, err := handler.Reload(testConfig(rotated, providerB), nil); err != nil {
		t.Fatal(err)
	}
	if !smartAvailable() {
		t.Fatal("a provider with a new API key inherited the old cooldown")
	}
	if got := postChat(t, server, chatBody("smart"), nil).header.Get("X-Gonka-Provider"); got != "a" {
		t.Fatalf("smart served by %q, want a after key rotation", got)
	}
}

func TestReloadRejectsInvalidConfig(t *testing.T) {
	a := newFakeUpstream(t)
	cfg := testConfig(config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 1, ModelAlias: "a-model"})
	handler, server := startProxy(t, cfg, proxy.Options{})
	_, before, _ := handler.Current()

	invalid := testConfig(config.Provider{Name: "a", BaseURL: a.URL, APIKey: "key-a", Priority: 1, ModelAlias: "a-model", Models: map[string]string{"v": "m"}})
	if _, err := handler.Reload(invalid, nil); err == nil {
		t.Fatal("Reload accepted a provider with both model_alias and models")
	}
	if _, after, _ := handler.Current(); after != before {
		t.Fatal("a failed reload changed the running config")
	}
	if response := postChat(t, server, chatBody("v"), nil); response.status != http.StatusOK {
		t.Fatalf("status after failed reload = %d", response.status)
	}

	hookErr := errors.New("disk full")
	other := testConfig(config.Provider{Name: "other", BaseURL: a.URL, APIKey: "key-o", Priority: 1, ModelAlias: "o"})
	if _, err := handler.Reload(other, func(config.Config) error { return hookErr }); !errors.Is(err, hookErr) {
		t.Fatalf("Reload with failing hook = %v, want %v", err, hookErr)
	}
	if _, after, _ := handler.Current(); after != before {
		t.Fatal("a reload whose hook failed changed the running config")
	}

	result, err := handler.Reload(cfg, nil)
	if err != nil || result.Changed || result.Version != before {
		t.Fatalf("reloading the same config = %+v, %v; want unchanged %s", result, err, before)
	}
}
