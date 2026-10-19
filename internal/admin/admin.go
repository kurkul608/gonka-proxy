// Package admin serves the runtime administration API: read the active
// configuration, replace it (hot reload) and inspect routing status.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/glaicer/gonka-proxy/internal/auth"
	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

// MinTokenLength is the shortest accepted admin token.
const MinTokenLength = 16

// MaxConfigBytes bounds a PUT /admin/config body.
const MaxConfigBytes = 1 << 20

const (
	configPath = "/admin/config"
	statusPath = "/admin/status"

	codeUnauthorized         = "invalid_admin_token"
	codeInvalidConfig        = "invalid_config"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeBodyTooLarge         = "request_too_large"
	codePersistFailed        = "persist_failed"
	codeNotFound             = "not_found"
	codeMethodNotAllowed     = "method_not_allowed"
)

// Logger is the operational logging surface used by the admin API.
type Logger interface {
	Printf(format string, args ...any)
}

// Options configure the admin API.
type Options struct {
	// Token is the bearer token every admin request must carry.
	Token string
	// ConfigPath is the configuration file; it is rewritten on a successful
	// PUT when Persist is set.
	ConfigPath string
	Persist    bool
	Logger     Logger
}

// Handler serves /admin/*.
type Handler struct {
	server  *proxy.Server
	options Options
}

// New validates the options and returns the admin handler.
func New(server *proxy.Server, options Options) (*Handler, error) {
	options.Token = strings.TrimSpace(options.Token)
	if len(options.Token) < MinTokenLength {
		return nil, fmt.Errorf("admin token must be at least %d characters", MinTokenLength)
	}
	if options.Persist && strings.TrimSpace(options.ConfigPath) == "" {
		return nil, errors.New("admin persistence needs a config file path")
	}
	if options.Logger == nil {
		options.Logger = discardLogger{}
	}
	return &Handler{server: server, options: options}, nil
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// ServeHTTP authenticates and dispatches admin requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !auth.BearerMatches(r.Header.Get("Authorization"), h.options.Token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gonka-proxy-admin"`)
		proxy.WriteError(w, http.StatusUnauthorized, proxy.ErrorTypeInvalidRequest, codeUnauthorized, "missing or invalid admin token")
		return
	}
	switch r.URL.Path {
	case configPath:
		switch r.Method {
		case http.MethodGet:
			h.getConfig(w)
		case http.MethodPut:
			h.putConfig(w, r)
		default:
			methodNotAllowed(w, "GET, PUT")
		}
	case statusPath:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, h.server.Status())
	default:
		proxy.WriteError(w, http.StatusNotFound, proxy.ErrorTypeInvalidRequest, codeNotFound, "unknown admin endpoint")
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	proxy.WriteError(w, http.StatusMethodNotAllowed, proxy.ErrorTypeInvalidRequest, codeMethodNotAllowed, "method not allowed")
}

// ConfigResponse is the body of GET /admin/config.
type ConfigResponse struct {
	Version  string          `json:"version"`
	LoadedAt string          `json:"loaded_at"`
	Config   config.Document `json:"config"`
}

// ApplyResponse is the body of a successful PUT /admin/config.
type ApplyResponse struct {
	Version   string `json:"version"`
	Changed   bool   `json:"changed"`
	Persisted bool   `json:"persisted"`
	LoadedAt  string `json:"loaded_at"`
}

func (h *Handler) getConfig(w http.ResponseWriter) {
	cfg, version, loadedAt := h.server.Current()
	writeJSON(w, http.StatusOK, ConfigResponse{
		Version:  version,
		LoadedAt: formatTime(loadedAt),
		Config:   cfg.Document().Redacted(),
	})
}

func (h *Handler) putConfig(w http.ResponseWriter, r *http.Request) {
	format, err := requestFormat(r.Header.Get("Content-Type"))
	if err != nil {
		proxy.WriteError(w, http.StatusUnsupportedMediaType, proxy.ErrorTypeInvalidRequest, codeUnsupportedMediaType, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxConfigBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			proxy.WriteError(w, http.StatusRequestEntityTooLarge, proxy.ErrorTypeInvalidRequest, codeBodyTooLarge,
				fmt.Sprintf("config body exceeds %d bytes", MaxConfigBytes))
			return
		}
		proxy.WriteError(w, http.StatusBadRequest, proxy.ErrorTypeInvalidRequest, codeInvalidConfig, "could not read request body")
		return
	}

	cfg, err := config.Parse(body, format, "request body")
	if err != nil {
		message := scrubValues(err.Error())
		h.options.Logger.Printf("admin config rejected - %s", message)
		proxy.WriteError(w, http.StatusBadRequest, proxy.ErrorTypeInvalidRequest, codeInvalidConfig, message)
		return
	}

	persisted := false
	var persistErr error
	result, err := h.server.Reload(cfg, func(next config.Config) error {
		if !h.options.Persist {
			return nil
		}
		data, err := next.MarshalFile()
		if err != nil {
			persistErr = err
			return err
		}
		if err := config.WriteFileAtomic(h.options.ConfigPath, data); err != nil {
			persistErr = err
			return err
		}
		persisted = true
		return nil
	})
	if err != nil {
		if persistErr != nil {
			h.options.Logger.Printf("admin config not applied - persist failed - %v", persistErr)
			proxy.WriteError(w, http.StatusInternalServerError, proxy.ErrorTypeServer, codePersistFailed,
				"could not write the config file; the running configuration is unchanged")
			return
		}
		message := scrubValues(err.Error())
		proxy.WriteError(w, http.StatusBadRequest, proxy.ErrorTypeInvalidRequest, codeInvalidConfig, message)
		return
	}
	writeJSON(w, http.StatusOK, ApplyResponse{
		Version:   result.Version,
		Changed:   result.Changed,
		Persisted: persisted,
		LoadedAt:  formatTime(result.LoadedAt),
	})
}

// requestFormat maps a Content-Type to a config format. JSON needs an
// explicit JSON media type; YAML is the default.
func requestFormat(contentType string) (config.Format, error) {
	if strings.TrimSpace(contentType) == "" {
		return config.FormatYAML, nil
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return 0, fmt.Errorf("invalid Content-Type")
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return config.FormatJSON, nil
	case mediaType == "application/yaml", mediaType == "application/x-yaml",
		mediaType == "text/yaml", mediaType == "text/x-yaml", mediaType == "text/plain":
		return config.FormatYAML, nil
	default:
		return 0, fmt.Errorf("Content-Type must be application/json or application/yaml")
	}
}

// backtickValue matches the `value` fragments YAML decode errors quote; they
// can contain whatever was misplaced in the document, secrets included.
var backtickValue = regexp.MustCompile("`[^`]*`")

func scrubValues(message string) string {
	return backtickValue.ReplaceAllString(message, "`…`")
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
