package admin_test

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glaicer/gonka-proxy/internal/admin"
	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

const adminToken = "admin-token-0123456789"

const initialConfig = `reasoning_effort: high
providers:
  - name: gate-a
    base_url: https://a.example/v1
    api_key: secret-key-a-1111
    priority: 10
    models:
      chat: real-chat
`

const updatedConfig = `reasoning_effort: low
models:
  chat:
    reasoning_effort: medium
providers:
  - name: gate-a
    base_url: https://a.example/v1
    api_key: secret-key-a-1111
    priority: 10
    models:
      chat: real-chat
  - name: gate-b
    base_url: https://b.example/v1
    api_key: secret-key-b-2222
    priority: 5
    enabled: false
    models:
      chat: real-chat-b
`

type fixture struct {
	server *proxy.Server
	http   *httptest.Server
	path   string
}

func newFixture(t *testing.T, persist bool) fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(initialConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	server, err := proxy.NewWithOptions(cfg, proxy.Options{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := admin.New(server, admin.Options{Token: adminToken, ConfigPath: path, Persist: persist, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return fixture{server: server, http: httpServer, path: path}
}

type adminResponse struct {
	status int
	header http.Header
	body   []byte
}

func (f fixture) do(t *testing.T, method, path, contentType, body, authorization string) adminResponse {
	t.Helper()
	request, err := http.NewRequest(method, f.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return adminResponse{status: response.StatusCode, header: response.Header, body: data}
}

func (f fixture) authed(t *testing.T, method, path, contentType, body string) adminResponse {
	t.Helper()
	return f.do(t, method, path, contentType, body, "Bearer "+adminToken)
}

func errorCode(t *testing.T, response adminResponse) (string, string) {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.body, &payload); err != nil {
		t.Fatalf("decode error %q: %v", response.body, err)
	}
	return payload.Error.Code, payload.Error.Message
}

func TestNewRejectsWeakTokens(t *testing.T) {
	cfg, err := config.Parse([]byte("reasoning_effort: max\n"), config.FormatYAML, "test")
	if err != nil {
		t.Fatal(err)
	}
	server, err := proxy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "short", "   fifteen-chars", strings.Repeat("x", admin.MinTokenLength-1)} {
		if _, err := admin.New(server, admin.Options{Token: token}); err == nil {
			t.Errorf("admin.New accepted token %q", token)
		}
	}
	if _, err := admin.New(server, admin.Options{Token: strings.Repeat("x", admin.MinTokenLength), Persist: true}); err == nil {
		t.Error("admin.New accepted persistence without a config path")
	}
}

func TestAdminAuthentication(t *testing.T) {
	f := newFixture(t, false)
	for name, authorization := range map[string]string{
		"missing":       "",
		"wrong token":   "Bearer admin-token-wrong-000000",
		"prefix":        "Bearer admin-token",
		"longer":        "Bearer " + adminToken + "0",
		"basic scheme":  "Basic " + adminToken,
		"bare token":    adminToken,
		"empty bearer":  "Bearer ",
		"client header": "Bearer client-token",
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/admin/config", "/admin/status", "/admin/unknown"} {
				response := f.do(t, http.MethodGet, path, "", "", authorization)
				if response.status != http.StatusUnauthorized {
					t.Fatalf("%s status = %d, want 401", path, response.status)
				}
				if code, _ := errorCode(t, response); code != "invalid_admin_token" {
					t.Fatalf("code = %q", code)
				}
				if response.header.Get("WWW-Authenticate") == "" {
					t.Fatal("missing WWW-Authenticate")
				}
			}
		})
	}
	if response := f.do(t, http.MethodGet, "/admin/status", "", "", "bearer "+adminToken); response.status != http.StatusOK {
		t.Fatalf("lower-case bearer status = %d", response.status)
	}
}

func TestAdminRouting(t *testing.T) {
	f := newFixture(t, false)
	if response := f.authed(t, http.MethodGet, "/admin/nope", "", ""); response.status != http.StatusNotFound {
		t.Fatalf("unknown endpoint status = %d", response.status)
	}
	response := f.authed(t, http.MethodPost, "/admin/config", "application/json", "{}")
	if response.status != http.StatusMethodNotAllowed || response.header.Get("Allow") != "GET, PUT" {
		t.Fatalf("POST /admin/config = %d Allow %q", response.status, response.header.Get("Allow"))
	}
	if response := f.authed(t, http.MethodDelete, "/admin/status", "", ""); response.status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /admin/status = %d", response.status)
	}
}

type configBody struct {
	Version  string          `json:"version"`
	LoadedAt string          `json:"loaded_at"`
	Config   config.Document `json:"config"`
}

type applyBody struct {
	Version   string `json:"version"`
	Changed   bool   `json:"changed"`
	Persisted bool   `json:"persisted"`
	LoadedAt  string `json:"loaded_at"`
}

func getConfig(t *testing.T, f fixture) configBody {
	t.Helper()
	response := f.authed(t, http.MethodGet, "/admin/config", "", "")
	if response.status != http.StatusOK {
		t.Fatalf("GET /admin/config = %d %s", response.status, response.body)
	}
	if strings.Contains(string(response.body), "secret-key") {
		t.Fatalf("GET /admin/config leaks an API key: %s", response.body)
	}
	var body configBody
	if err := json.Unmarshal(response.body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func putConfig(t *testing.T, f fixture, contentType, body string) (adminResponse, applyBody) {
	t.Helper()
	response := f.authed(t, http.MethodPut, "/admin/config", contentType, body)
	var applied applyBody
	if response.status == http.StatusOK {
		if err := json.Unmarshal(response.body, &applied); err != nil {
			t.Fatal(err)
		}
	}
	return response, applied
}

func TestGetConfigRedactsKeys(t *testing.T) {
	f := newFixture(t, false)
	body := getConfig(t, f)
	_, version, _ := f.server.Current()
	if body.Version != version || len(body.Version) != 64 {
		t.Fatalf("version = %q, want %q", body.Version, version)
	}
	if got := body.Config.Providers[0].APIKey; got != "****1111" {
		t.Fatalf("api_key = %q, want ****1111", got)
	}
	if body.Config.Providers[0].Models["chat"] != "real-chat" || body.Config.ReasoningEffort == nil || *body.Config.ReasoningEffort != "high" {
		t.Fatalf("config = %+v", body.Config)
	}
}

func TestPutConfigAppliesAndVersions(t *testing.T) {
	f := newFixture(t, false)
	before := getConfig(t, f).Version

	response, applied := putConfig(t, f, "application/yaml", updatedConfig)
	if response.status != http.StatusOK {
		t.Fatalf("PUT YAML = %d %s", response.status, response.body)
	}
	if !applied.Changed || applied.Version == before || applied.Persisted {
		t.Fatalf("PUT result = %+v, want changed, new version, not persisted", applied)
	}
	after := getConfig(t, f)
	if after.Version != applied.Version || len(after.Config.Providers) != 2 || after.Config.Providers[1].Enabled {
		t.Fatalf("GET after PUT = %+v", after)
	}
	status := f.server.Status()
	if status.Version != applied.Version || status.Providers != 2 || status.EnabledProviders != 1 {
		t.Fatalf("status = %+v", status)
	}

	// The same config again, as YAML or JSON, is a no-op with a stable version.
	response, again := putConfig(t, f, "", updatedConfig)
	if response.status != http.StatusOK || again.Changed || again.Version != applied.Version {
		t.Fatalf("re-PUT = %d %+v", response.status, again)
	}
	cfg, _, _ := f.server.Current()
	jsonBody, err := json.Marshal(cfg.Document())
	if err != nil {
		t.Fatal(err)
	}
	response, viaJSON := putConfig(t, f, "application/json; charset=utf-8", string(jsonBody))
	if response.status != http.StatusOK || viaJSON.Changed || viaJSON.Version != applied.Version {
		t.Fatalf("JSON PUT = %d %s %+v", response.status, response.body, viaJSON)
	}
	if current, _ := os.ReadFile(f.path); string(current) != initialConfig {
		t.Fatal("config file changed although persistence is off")
	}
}

func TestPutConfigRejectsInvalidBodies(t *testing.T) {
	f := newFixture(t, false)
	before := getConfig(t, f).Version
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{"both alias and models", "application/yaml", strings.Replace(initialConfig, "    models:", "    model_alias: x\n    models:", 1), http.StatusBadRequest, "invalid_config"},
		{"unknown field", "application/yaml", initialConfig + "extra: true\n", http.StatusBadRequest, "invalid_config"},
		{"missing reasoning_effort", "application/json", `{"providers":[]}`, http.StatusBadRequest, "invalid_config"},
		{"malformed JSON", "application/json", `{"reasoning_effort":`, http.StatusBadRequest, "invalid_config"},
		{"YAML sent as JSON", "application/json", initialConfig, http.StatusBadRequest, "invalid_config"},
		{"unsupported media type", "application/xml", "<config/>", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"too large", "application/yaml", "reasoning_effort: max\n#" + strings.Repeat("x", admin.MaxConfigBytes), http.StatusRequestEntityTooLarge, "request_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, _ := putConfig(t, f, test.contentType, test.body)
			if response.status != test.status {
				t.Fatalf("status = %d, want %d (%s)", response.status, test.status, response.body)
			}
			if code, _ := errorCode(t, response); code != test.code {
				t.Fatalf("code = %q, want %q", code, test.code)
			}
		})
	}
	if after := getConfig(t, f).Version; after != before {
		t.Fatal("a rejected PUT changed the running config")
	}
}

func TestPutConfigErrorsDoNotEchoValues(t *testing.T) {
	f := newFixture(t, false)
	body := strings.Replace(initialConfig, "priority: 10", "priority: sk-live-very-secret", 1)
	response, _ := putConfig(t, f, "application/yaml", body)
	if response.status != http.StatusBadRequest {
		t.Fatalf("status = %d", response.status)
	}
	if strings.Contains(string(response.body), "sk-live-very-secret") {
		t.Fatalf("error echoes a config value: %s", response.body)
	}
}

func TestPutConfigPersists(t *testing.T) {
	f := newFixture(t, true)
	response, applied := putConfig(t, f, "application/yaml", updatedConfig)
	if response.status != http.StatusOK || !applied.Persisted {
		t.Fatalf("PUT = %d %s", response.status, response.body)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("persisted mode = %o", info.Mode().Perm())
	}
	persisted, err := config.Load(f.path)
	if err != nil {
		t.Fatalf("persisted file does not load: %v", err)
	}
	if persisted.Version() != applied.Version {
		t.Fatal("persisted file has a different version than the applied config")
	}
}

func TestPutConfigPersistFailureKeepsRunningConfig(t *testing.T) {
	f := newFixture(t, false)
	before := getConfig(t, f).Version
	handler, err := admin.New(f.server, admin.Options{
		Token:      adminToken,
		ConfigPath: filepath.Join(t.TempDir(), "missing-dir", "config.yaml"),
		Persist:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	broken := fixture{server: f.server, http: server}
	response, _ := putConfig(t, broken, "application/yaml", updatedConfig)
	if response.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", response.status, response.body)
	}
	if code, _ := errorCode(t, response); code != "persist_failed" {
		t.Fatalf("code = %q", code)
	}
	if after := getConfig(t, f).Version; after != before {
		t.Fatal("a config that could not be persisted was applied")
	}
}

func TestStatusEndpoint(t *testing.T) {
	f := newFixture(t, false)
	response := f.authed(t, http.MethodGet, "/admin/status", "", "")
	if response.status != http.StatusOK {
		t.Fatalf("status = %d", response.status)
	}
	var status struct {
		Version          string `json:"version"`
		LoadedAt         string `json:"loaded_at"`
		Providers        int    `json:"providers"`
		EnabledProviders int    `json:"enabled_providers"`
		ProviderStatus   []struct {
			Name          string  `json:"name"`
			Priority      int     `json:"priority"`
			Enabled       bool    `json:"enabled"`
			Available     bool    `json:"available"`
			CooldownUntil *string `json:"cooldown_until"`
			Models        []struct {
				Virtual   string `json:"virtual"`
				Upstream  string `json:"upstream"`
				Available bool   `json:"available"`
			} `json:"models"`
		} `json:"provider_status"`
	}
	if err := json.Unmarshal(response.body, &status); err != nil {
		t.Fatal(err)
	}
	if status.Providers != 1 || status.EnabledProviders != 1 || status.LoadedAt == "" || len(status.Version) != 64 {
		t.Fatalf("status = %+v", status)
	}
	provider := status.ProviderStatus[0]
	if provider.Name != "gate-a" || provider.Priority != 10 || !provider.Enabled || !provider.Available || provider.CooldownUntil != nil {
		t.Fatalf("provider = %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].Virtual != "chat" || provider.Models[0].Upstream != "real-chat" || !provider.Models[0].Available {
		t.Fatalf("models = %+v", provider.Models)
	}
	if strings.Contains(string(response.body), "secret-key") {
		t.Fatal("status leaks an API key")
	}
}
