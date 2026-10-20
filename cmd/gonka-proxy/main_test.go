package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRunRejectsInvalidStartupConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantErr  string
	}{
		{
			name:     "malformed YAML",
			contents: "reasoning_effort: null\nproviders:\n  - [",
			wantErr:  "load configuration: decode config",
		},
		{
			name: "invalid duration",
			contents: `reasoning_effort: null
cooldown: soon
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 10
`,
			wantErr: "cooldown must be a valid duration",
		},
		{
			name: "missing Provider field",
			contents: `reasoning_effort: null
providers:
  - name: primary
    base_url: https://provider.example/v1
    model_alias: provider-model
    priority: 10
`,
			wantErr: "providers[0].api_key must not be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			err := run(context.Background(), configPath, log.New(io.Discard, "", 0))
			if err == nil {
				t.Fatal("run succeeded for invalid configuration")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestRunDuplicateProviderNameDoesNotLeakAPIKey(t *testing.T) {
	const secret = "provider-secret-key"
	contents := `reasoning_effort: null
providers:
  - name: "` + secret + `"
    base_url: https://primary.example/v1
    api_key: ` + secret + `
    model_alias: primary-model
    priority: 10
  - name: " ` + secret + ` "
    base_url: https://backup.example/v1
    api_key: backup-secret-key
    model_alias: backup-model
    priority: 5
`
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var logs bytes.Buffer
	err := run(context.Background(), configPath, log.New(&logs, "", 0))
	if err == nil {
		t.Fatal("run succeeded for duplicate Provider names")
	}
	if !strings.Contains(err.Error(), "providers[1].name duplicates providers[0].name") {
		t.Fatalf("error = %q, want actionable duplicate-index error", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked API key %q: %q", secret, err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("startup logs leaked API key %q: %q", secret, logs.String())
	}
}

func TestRunShutsDownActiveRoutingOnContextCancellation(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startedOnce.Do(func() {
			close(upstreamStarted)
		})
		<-r.Context().Done()
		canceledOnce.Do(func() {
			close(upstreamCanceled)
		})
	}))
	defer provider.Close()

	listenAddress := reserveTCPAddress(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf(`server:
  listen_address: %s
cooldown: 1h
recovery_wait: 1h
response_header_timeout: 1h
reasoning_effort: max
providers:
  - name: primary
    base_url: %s/v1
    api_key: provider-secret
    model_alias: provider-model
    priority: 1
`, listenAddress, provider.URL)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	runResult := make(chan error, 1)
	go func() {
		runResult <- run(ctx, configPath, log.New(&logs, "", 0))
	}()
	waitForTCP(t, listenAddress)

	requestResult := make(chan error, 1)
	go func() {
		request, err := http.NewRequest(http.MethodPost, "http://"+listenAddress+"/v1/chat/completions", strings.NewReader(`{"model":"virtual-model","messages":[]}`))
		if err != nil {
			requestResult <- err
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active upstream routing")
	}
	cancel()

	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatalf("shutdown did not cancel the active upstream request; logs = %q", logs.String())
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not finish after cancellation")
	}
	select {
	case <-requestResult:
	case <-time.After(time.Second):
		t.Fatal("downstream request did not finish after shutdown")
	}
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP address: %v", err)
	}
	return address
}

func waitForTCP(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", address)
}

type runningProxy struct {
	proxyURL string
	adminURL string
	reload   chan os.Signal
	logs     *syncBuffer
	cancel   context.CancelFunc
	done     chan error
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startRun(t *testing.T, configPath string, env map[string]string) *runningProxy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	running := &runningProxy{
		reload: make(chan os.Signal, 1),
		logs:   &syncBuffer{},
		cancel: cancel,
		done:   make(chan error, 1),
	}
	ready := make(chan struct{})
	go func() {
		running.done <- runWith(ctx, configPath, log.New(running.logs, "", 0), runOptions{
			getenv: func(key string) string { return env[key] },
			reload: running.reload,
			ready: func(proxyAddress, adminAddress string) {
				running.proxyURL = "http://" + proxyAddress
				if adminAddress != "" {
					running.adminURL = "http://" + adminAddress
				}
				close(ready)
			},
		})
	}()
	select {
	case <-ready:
	case err := <-running.done:
		cancel()
		t.Fatalf("run exited early: %v", err)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("run did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-running.done:
			if err != nil {
				t.Errorf("run returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("run did not stop")
		}
	})
	return running
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func legacyConfig(alias string) string {
	return `server:
  listen_address: 127.0.0.1:0
reasoning_effort: max
providers:
  - name: primary
    base_url: https://provider.example/v1
    api_key: provider-secret-key
    model_alias: ` + alias + `
    priority: 1
`
}

func httpGet(t *testing.T, url, authorization string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func TestRunReloadsConfigOnSIGHUP(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, legacyConfig("model-a"))
	running := startRun(t, configPath, nil)

	if _, body := httpGet(t, running.proxyURL+"/v1/models", ""); !strings.Contains(body, `"model-a"`) {
		t.Fatalf("models before reload = %s", body)
	}

	writeConfig(t, configPath, legacyConfig("model-b"))
	running.reload <- syscall.SIGHUP
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, body := httpGet(t, running.proxyURL+"/v1/models", "")
		if strings.Contains(body, `"model-b"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP did not reload the config; models = %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	writeConfig(t, configPath, "reasoning_effort: extreme\n")
	running.reload <- syscall.SIGHUP
	deadline = time.Now().Add(2 * time.Second)
	for !strings.Contains(running.logs.String(), "config reload failed") {
		if time.Now().After(deadline) {
			t.Fatalf("invalid reload was not logged; logs = %q", running.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, body := httpGet(t, running.proxyURL+"/v1/models", ""); !strings.Contains(body, `"model-b"`) {
		t.Fatalf("a failed reload replaced the running config; models = %s", body)
	}
}

func TestRunAdminAPIWiring(t *testing.T) {
	const token = "admin-token-0123456789"
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, legacyConfig("model-a"))

	t.Run("disabled without a token", func(t *testing.T) {
		running := startRun(t, configPath, nil)
		if status, _ := httpGet(t, running.proxyURL+"/admin/status", "Bearer "+token); status != http.StatusNotFound {
			t.Fatalf("/admin/status = %d, want 404", status)
		}
	})
	t.Run("same listener", func(t *testing.T) {
		running := startRun(t, configPath, map[string]string{envAdminToken: token})
		if status, body := httpGet(t, running.proxyURL+"/admin/status", "Bearer "+token); status != http.StatusOK {
			t.Fatalf("/admin/status = %d %s", status, body)
		}
		if status, _ := httpGet(t, running.proxyURL+"/admin/status", ""); status != http.StatusUnauthorized {
			t.Fatalf("/admin/status without token = %d", status)
		}
	})
	t.Run("separate listener", func(t *testing.T) {
		running := startRun(t, configPath, map[string]string{envAdminToken: token, envAdminListenAddress: "127.0.0.1:0"})
		if status, _ := httpGet(t, running.proxyURL+"/admin/status", "Bearer "+token); status != http.StatusNotFound {
			t.Fatalf("/admin/status on the proxy port = %d, want 404", status)
		}
		if status, body := httpGet(t, running.adminURL+"/admin/status", "Bearer "+token); status != http.StatusOK {
			t.Fatalf("/admin/status on the admin port = %d %s", status, body)
		}
		if status, _ := httpGet(t, running.adminURL+"/v1/models", ""); status != http.StatusNotFound {
			t.Fatalf("/v1/models on the admin port = %d, want 404", status)
		}
	})
	t.Run("client token", func(t *testing.T) {
		running := startRun(t, configPath, map[string]string{envClientToken: "client-token-0123"})
		if status, _ := httpGet(t, running.proxyURL+"/v1/models", ""); status != http.StatusUnauthorized {
			t.Fatalf("/v1/models without client token = %d", status)
		}
		if status, _ := httpGet(t, running.proxyURL+"/v1/models", "Bearer client-token-0123"); status != http.StatusOK {
			t.Fatalf("/v1/models with client token = %d", status)
		}
	})
}

func TestRunRejectsInvalidAdminEnvironment(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, legacyConfig("model-a"))
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"short admin token", map[string]string{envAdminToken: "short"}, "at least 16 characters"},
		{"admin address without token", map[string]string{envAdminListenAddress: "127.0.0.1:0"}, envAdminToken + " is not"},
		{"invalid persist flag", map[string]string{envAdminToken: "admin-token-0123456789", envAdminPersist: "sometimes"}, envAdminPersist},
		{"invalid stats window", map[string]string{envStatsWindow: "soon"}, envStatsWindow},
		{"too short stats window", map[string]string{envStatsWindow: "10ms"}, envStatsWindow},
		{"invalid stats alpha", map[string]string{envStatsEWMAAlpha: "1.5"}, envStatsEWMAAlpha},
		{"zero stats alpha", map[string]string{envStatsEWMAAlpha: "0"}, envStatsEWMAAlpha},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := runWith(context.Background(), configPath, log.New(io.Discard, "", 0), runOptions{
				getenv: func(key string) string { return test.env[key] },
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
			if strings.Contains(err.Error(), "admin-token-0123456789") {
				t.Fatalf("error leaks the admin token: %v", err)
			}
		})
	}
}

func TestRunStartsWithZeroProviders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, configPath, "server:\n  listen_address: 127.0.0.1:0\nreasoning_effort: max\nproviders: []\n")
	running := startRun(t, configPath, nil)
	response, err := http.Post(running.proxyURL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"no_provider_available"`) {
		t.Fatalf("status %d body %s", response.StatusCode, body)
	}
}
