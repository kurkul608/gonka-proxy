package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glaicer/gonka-proxy/internal/auth"
	"github.com/glaicer/gonka-proxy/internal/config"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	modelsPath          = "/v1/models"

	// MaxRoutingTimeHeader lets a client shorten the Max Routing Time for one
	// request (capped by max_routing_time when that is configured).
	MaxRoutingTimeHeader = "X-Gonka-Max-Routing-Time"
	// ProviderHeader and ModelHeader name the Provider and upstream model that
	// served a response.
	ProviderHeader = "X-Gonka-Provider"
	ModelHeader    = "X-Gonka-Model"
)

const (
	maxLoggedErrorMessageLength = 512
	sseDoneMarker               = "[DONE]"
	streamAbortBuf              = 64
	sseDoneLineMax              = len("data: [DONE]") + 1 // include an optional CR

	// Error responses are read only long enough to extract a concise INFO log
	// message. A timer closes the response body if an upstream stalls after
	// sending failover headers.
	maxErrorBodyBytes    = 64 * 1024
	errorBodyReadTimeout = 100 * time.Millisecond
)

// Logger is the operational logging surface used by the proxy.
type Logger interface {
	Printf(format string, args ...any)
}

// Options are process-level settings that do not belong to the hot-reloadable
// configuration.
type Options struct {
	Logger Logger
	// ClientToken, when set, is the bearer token every /v1/* request must carry.
	ClientToken string
}

// Server implements the public OpenAI-compatible HTTP endpoint. Its routing
// configuration can be replaced at runtime with Reload.
type Server struct {
	state       atomic.Pointer[snapshot]
	cooldowns   *cooldownRegistry
	logger      Logger
	clientToken string
	reloadMu    sync.Mutex
}

// New creates a proxy handler from validated configuration.
func New(cfg config.Config) (*Server, error) {
	return NewWithOptions(cfg, Options{Logger: log.Default()})
}

// NewWithLogger creates a proxy handler with an injected operational logger.
func NewWithLogger(cfg config.Config, logger Logger) (*Server, error) {
	return NewWithOptions(cfg, Options{Logger: logger})
}

// NewWithOptions creates a proxy handler with process-level options.
func NewWithOptions(cfg config.Config, options Options) (*Server, error) {
	logger := options.Logger
	if logger == nil {
		logger = log.Default()
	}
	st, err := buildSnapshot(cfg, logger, nil)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cooldowns:   newCooldownRegistry(),
		logger:      logger,
		clientToken: strings.TrimSpace(options.ClientToken),
	}
	s.state.Store(st)
	return s, nil
}

// ReloadResult describes the configuration in force after a Reload.
type ReloadResult struct {
	Version  string
	Changed  bool
	LoadedAt time.Time
}

// Reload validates cfg and atomically replaces the routing configuration.
// Requests already in flight finish on the configuration they started with;
// new requests use cfg. Cooldowns of Providers whose name, base URL and API
// key are unchanged (and of upstream models they still serve) are kept.
//
// beforeSwap, when not nil, runs after validation and before the swap (for
// example to persist cfg); an error from it aborts the reload.
func (s *Server) Reload(cfg config.Config, beforeSwap func(config.Config) error) (ReloadResult, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	current := s.state.Load()
	next, err := buildSnapshot(cfg, s.logger, current)
	if err != nil {
		return ReloadResult{}, err
	}
	if beforeSwap != nil {
		if err := beforeSwap(cfg); err != nil {
			return ReloadResult{}, err
		}
	}
	if next.version == current.version {
		return ReloadResult{Version: current.version, Changed: false, LoadedAt: current.loadedAt}, nil
	}
	if cfg.ListenAddress != current.cfg.ListenAddress {
		s.logger.Printf("config reload - server.listen_address change takes effect after a restart")
	}
	s.state.Store(next)
	s.cooldowns.retain(next.servesKey)
	if next.transport != current.transport {
		current.transport.CloseIdleConnections()
	}
	s.logger.Printf("config reloaded - version=%s providers=%d enabled=%d", shortVersion(next.version), len(next.providers), next.enabledCount)
	return ReloadResult{Version: next.version, Changed: true, LoadedAt: next.loadedAt}, nil
}

func shortVersion(version string) string {
	if len(version) > 12 {
		return version[:12]
	}
	return version
}

// Current returns the configuration in force, its version and load time.
func (s *Server) Current() (config.Config, string, time.Time) {
	st := s.state.Load()
	return st.cfg, st.version, st.loadedAt
}

// normalizeReasoningEffort normalizes an effort pointer so programmatic
// Config values like "MAX" behave like loaded YAML values.
func normalizeReasoningEffort(effort *config.ReasoningEffort) *config.ReasoningEffort {
	if effort == nil {
		return nil
	}
	n := effort.Normalize()
	return &n
}

// ServeHTTP handles chat completions and the model list used by client discovery.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.clientToken != "" && strings.HasPrefix(r.URL.Path, "/v1/") &&
		!auth.BearerMatches(r.Header.Get("Authorization"), s.clientToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gonka-proxy"`)
		WriteError(w, http.StatusUnauthorized, ErrorTypeInvalidRequest, CodeInvalidAPIKey, "missing or invalid client token")
		return
	}

	st := s.state.Load()
	switch r.URL.Path {
	case modelsPath:
		serveModels(st, w, r)
	case chatCompletionsPath:
		s.serveChat(st, w, r)
	default:
		http.NotFound(w, r)
	}
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// serveModels lists the configured Virtual Models (or, with only legacy
// Providers, each Model Alias once in priority order) so OpenAI-compatible
// clients can discover and test the endpoint.
func serveModels(st *snapshot, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	ids := st.listedModels()
	models := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		models = append(models, modelEntry{ID: id, Object: "model", OwnedBy: "gonka-proxy"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}{Object: "list", Data: models})
}

// chatRequest is one client request bound to the snapshot it started on.
type chatRequest struct {
	st           *snapshot
	r            *http.Request
	payload      map[string]json.RawMessage
	virtual      string
	routes       []route
	clientEffort *config.ReasoningEffort
	deadline     *routingDeadline
	limit        time.Duration
}

func (s *Server) serveChat(st *snapshot, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if st.enabledCount == 0 {
		WriteError(w, http.StatusServiceUnavailable, ErrorTypeServer, CodeNoProviderAvailable, "no Providers configured")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if r.Context().Err() != nil {
			st.logCancellation("request-body")
			return
		}
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, CodeInvalidRequestBody, "could not read request body")
		return
	}
	defer r.Body.Close()

	payload, err := decodeRequestBody(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, CodeInvalidRequestBody, "request body must be a JSON object")
		return
	}

	virtual := requestedModel(payload)
	routes := st.routes(virtual)
	if len(routes) == 0 {
		if !st.knows(virtual) {
			writeErrorParam(w, http.StatusNotFound, ErrorTypeInvalidRequest, CodeModelNotFound, "model",
				fmt.Sprintf("The model `%s` does not exist or is not served by this proxy.", virtual))
			return
		}
		WriteError(w, http.StatusServiceUnavailable, ErrorTypeServer, CodeNoProviderAvailable,
			fmt.Sprintf("no enabled Provider serves the model `%s`", virtual))
		return
	}

	var clientEffort *config.ReasoningEffort
	if st.clientEffortAllowed(virtual) {
		clientEffort, err = clientReasoningEffort(payload)
		if err != nil {
			writeErrorParam(w, http.StatusBadRequest, ErrorTypeInvalidRequest, CodeInvalidReasoningEffort, "reasoning_effort", err.Error())
			return
		}
	}

	limit, err := routingLimit(st.maxRoutingTime, r.Header.Get(MaxRoutingTimeHeader))
	if err != nil {
		WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, CodeInvalidMaxRoutingTime, err.Error())
		return
	}
	deadline := newRoutingDeadline(limit)
	defer deadline.stop()

	s.route(w, &chatRequest{
		st:           st,
		r:            r,
		payload:      payload,
		virtual:      virtual,
		routes:       routes,
		clientEffort: clientEffort,
		deadline:     deadline,
		limit:        limit,
	})
}

// requestedModel returns the client's Virtual Model, or "" when absent.
func requestedModel(payload map[string]json.RawMessage) string {
	var model string
	if raw, ok := payload["model"]; ok && json.Unmarshal(raw, &model) == nil {
		return strings.TrimSpace(model)
	}
	return ""
}

// clientReasoningEffort reads an allowed client-supplied reasoning_effort.
// Absent or JSON null means "not supplied".
func clientReasoningEffort(payload map[string]json.RawMessage) (*config.ReasoningEffort, error) {
	raw, ok := payload["reasoning_effort"]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errors.New(config.ReasoningEffortErrorMessage)
	}
	effort, err := config.ParseReasoningEffort(value)
	if err != nil {
		return nil, errors.New(config.ReasoningEffortErrorMessage)
	}
	return effort, nil
}

// routingLimit combines max_routing_time with the optional per-request
// header. The header may only shorten a configured limit. A bare number is
// read as seconds.
func routingLimit(configured time.Duration, header string) (time.Duration, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return configured, nil
	}
	requested, err := time.ParseDuration(header)
	if err != nil {
		seconds, numberErr := strconv.ParseFloat(header, 64)
		if numberErr != nil {
			return 0, fmt.Errorf("%s must be a duration such as 10s", MaxRoutingTimeHeader)
		}
		requested = time.Duration(seconds * float64(time.Second))
	}
	if requested <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", MaxRoutingTimeHeader)
	}
	if configured > 0 && requested > configured {
		return configured, nil
	}
	return requested, nil
}

func (s *Server) route(w http.ResponseWriter, req *chatRequest) {
	st := req.st
	for {
		if req.r.Context().Err() != nil {
			st.logCancellation("routing")
			return
		}

		for _, candidate := range req.routes {
			if req.r.Context().Err() != nil {
				st.logCancellation("routing")
				return
			}
			if req.deadline.isExpired() {
				writeRoutingTimeout(st, w, req.limit)
				return
			}
			if !s.cooldowns.available(candidate.provider.id, candidate.upstream, time.Now()) {
				continue
			}
			st.logAt(config.LogLevelInfo, "provider selected - %s - priority=%d", st.redactProviderSecrets(candidate.provider.Name), candidate.provider.Priority)
			if s.attempt(w, req, candidate) {
				return
			}
		}

		if !s.waitForRecovery(req) {
			if req.deadline.isExpired() && req.r.Context().Err() == nil {
				writeRoutingTimeout(st, w, req.limit)
			}
			return
		}
	}
}

// attempt sends the request to one Provider. It returns true when the
// request is finished (response written, client gone, or routing time
// exceeded) and false when routing should continue with the next Provider.
func (s *Server) attempt(w http.ResponseWriter, req *chatRequest, candidate route) bool {
	st := req.st
	r := req.r
	selected := candidate.provider

	upstreamBody, err := applyUpstreamOverrides(req.payload, candidate.upstream, st.resolveEffort(selected, req.virtual, req.clientEffort))
	if err != nil {
		WriteError(w, http.StatusBadGateway, ErrorTypeServer, CodeUpstreamRequestFailed, "could not encode upstream request")
		return true
	}

	attemptContext, cancelAttempt := context.WithCancel(r.Context())
	defer cancelAttempt()
	req.deadline.arm(cancelAttempt)

	upstreamRequest, err := http.NewRequestWithContext(attemptContext, http.MethodPost, selected.chatURL, bytes.NewReader(upstreamBody))
	if err != nil {
		WriteError(w, http.StatusBadGateway, ErrorTypeServer, CodeUpstreamRequestFailed, "could not create upstream request")
		return true
	}
	copyHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Del(MaxRoutingTimeHeader)
	upstreamRequest.Header.Set("Authorization", "Bearer "+selected.APIKey)

	upstreamResponse, err := st.client.Do(upstreamRequest)
	if err != nil {
		if r.Context().Err() != nil {
			st.logCancellation("upstream")
			return true
		}
		if req.deadline.isExpired() {
			writeRoutingTimeout(st, w, req.limit)
			return true
		}
		s.logProviderResponse(st, selected, 0, err.Error())
		kind, category := failureKindNetwork, "network"
		if isResponseHeaderTimeout(err) {
			kind, category = failureKindTimeout, "response-header-timeout"
		}
		s.handleFailoverFailure(st, candidate, kind, category, 0)
		return false
	}

	statusCode := upstreamResponse.StatusCode
	if statusCode != http.StatusOK {
		authFailure := st.failoverOnAuth && (statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden)
		if isFailoverStatus(statusCode) || (st.failoverOn404 && statusCode == http.StatusNotFound) || authFailure {
			var errorMessage string
			if st.payloadLoggingEnabled() {
				responseBody, readErr := readErrorBodyForLog(upstreamResponse.Body)
				if readErr == nil && len(responseBody) > 0 {
					errorMessage = errorMessageFromResponse(responseBody, statusCode)
				}
			}
			_ = upstreamResponse.Body.Close()
			s.logProviderResponse(st, selected, statusCode, errorMessage)

			category := "server-error"
			switch statusCode {
			case http.StatusPaymentRequired:
				category = "payment-required"
			case http.StatusTooManyRequests:
				category = "rate-limit"
			case http.StatusNotFound:
				category = "not-found"
			case http.StatusUnauthorized, http.StatusForbidden:
				category = "auth"
			}
			s.handleFailoverFailure(st, candidate, failureKindStatus, category, statusCode)
			return false
		}

		responseBody, readErr := io.ReadAll(upstreamResponse.Body)
		_ = upstreamResponse.Body.Close()
		errorMessage := ""
		if readErr != nil {
			errorMessage = fmt.Sprintf("read upstream error response: %v", readErr)
		} else if len(responseBody) > 0 {
			errorMessage = errorMessageFromResponse(responseBody, statusCode)
		}
		s.logProviderResponse(st, selected, statusCode, errorMessage)

		if isReasoningEffortUnsupportedError(statusCode, errorMessage, responseBody) {
			s.handleFailoverFailure(st, candidate, failureKindStatus, "reasoning-effort", statusCode)
			return false
		}
		if !req.deadline.commit() {
			writeRoutingTimeout(st, w, req.limit)
			return true
		}

		copyHeaders(w.Header(), upstreamResponse.Header)
		setProviderHeaders(w.Header(), selected.Name, candidate.upstream)
		w.WriteHeader(statusCode)
		_, _ = w.Write(responseBody)
		return true
	}

	defer upstreamResponse.Body.Close()
	if !req.deadline.commit() {
		writeRoutingTimeout(st, w, req.limit)
		return true
	}
	s.logProviderResponse(st, selected, statusCode, "")

	streaming := isStreamingResponse(upstreamResponse, req.payload)
	copyHeaders(w.Header(), upstreamResponse.Header)
	setProviderHeaders(w.Header(), selected.Name, candidate.upstream)
	w.WriteHeader(statusCode)
	if streaming {
		s.forwardStreamingResponse(w, req, candidate, statusCode, upstreamResponse.Body)
		return true
	}
	_, _ = io.Copy(w, upstreamResponse.Body)
	return true
}

func writeRoutingTimeout(st *snapshot, w http.ResponseWriter, limit time.Duration) {
	st.logAt(config.LogLevelWarn, "routing time exceeded - %s", limit)
	WriteError(w, http.StatusServiceUnavailable, ErrorTypeServer, CodeNoProviderAvailable,
		fmt.Sprintf("no Provider answered within the max routing time (%s)", limit))
}

// waitForRecovery runs one Recovery Wait. It returns false when the client
// went away or the routing time ran out.
func (s *Server) waitForRecovery(req *chatRequest) bool {
	st := req.st
	ctx := req.r.Context()
	versions := s.cooldowns.versions()
	st.logAt(config.LogLevelInfo, "recovery wait - started - %s", st.recoveryWait)
	timer := time.NewTimer(st.recoveryWait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		st.logAt(config.LogLevelInfo, "recovery wait - canceled")
		return false
	case <-req.deadline.expiredC():
		st.logAt(config.LogLevelInfo, "recovery wait - routing time exceeded")
		return false
	case <-timer.C:
		if ctx.Err() != nil {
			st.logAt(config.LogLevelInfo, "recovery wait - canceled")
			return false
		}
		cleared := s.cooldowns.clear(versions)
		for _, key := range cleared {
			st.logAt(config.LogLevelInfo, "%s - cooldown cleared", st.label(key))
		}
		st.logAt(config.LogLevelInfo, "recovery wait - completed - cleared=%d", len(cleared))
		return true
	}
}

// dataChunk is a small accumulator used to detect SSE termination markers that
// may straddle two read() boundaries.
type dataChunk struct {
	buf          []byte
	line         []byte
	started      bool
	done         bool
	lineTooLong  bool
	rawTailLimit int
}

func newDataChunk(rawTailLimit int) *dataChunk {
	if rawTailLimit < streamAbortBuf {
		rawTailLimit = streamAbortBuf
	}
	return &dataChunk{rawTailLimit: rawTailLimit}
}

func (c *dataChunk) observe(p []byte) {
	c.buf = append(c.buf, p...)
	if len(c.buf) > c.rawTailLimit {
		c.buf = c.buf[len(c.buf)-c.rawTailLimit:]
	}
	if len(c.buf) > 0 {
		c.started = true
	}

	for _, value := range p {
		if c.done {
			continue
		}
		if value == '\n' {
			if !c.lineTooLong && isSSEDoneLine(c.line) {
				c.done = true
			}
			c.line = c.line[:0]
			c.lineTooLong = false
			continue
		}
		if c.lineTooLong {
			continue
		}
		if len(c.line) >= sseDoneLineMax {
			c.lineTooLong = true
			continue
		}
		c.line = append(c.line, value)
	}
}

func (c *dataChunk) sawDone() bool {
	if !c.started || c.lineTooLong {
		return false
	}
	return c.done || isSSEDoneLine(c.line)
}

func isSSEDoneLine(line []byte) bool {
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return bytes.Equal(line, []byte("data:"+sseDoneMarker)) ||
		bytes.Equal(line, []byte("data: "+sseDoneMarker))
}

func (s *Server) forwardStreamingResponse(w http.ResponseWriter, req *chatRequest, candidate route, statusCode int, body io.Reader) {
	st := req.st
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	buffer := make([]byte, 32*1024)
	var sentBytes int64
	var chunks int64
	acc := newDataChunk(st.streamTailLimit)

	for {
		readCount, readErr := body.Read(buffer)
		if readCount > 0 {
			chunk := buffer[:readCount]
			acc.observe(chunk)
			sentBytes += int64(readCount)
			chunks++
			if _, writeErr := w.Write(chunk); writeErr != nil {
				recordStreamAbort(st, candidate.provider, statusCode, sentBytes, chunks, acc,
					fmt.Sprintf("client-write error: %v", writeErr))
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			if acc.sawDone() {
				return
			}
			recordStreamAbort(st, candidate.provider, statusCode, sentBytes, chunks, acc,
				"stream ended at EOF without [DONE] marker (broker SILENTLY cut the response)")
			s.markCooldown(st, candidate, scopeModel, &failureRecord{kind: failureKindStream, category: "stream-incomplete", status: statusCode})
			return
		}
		if req.r.Context().Err() != nil {
			recordStreamAbort(st, candidate.provider, statusCode, sentBytes, chunks, acc,
				fmt.Sprintf("upstream read error: %v (request context canceled during stream)", readErr))
			return
		}
		recordStreamAbort(st, candidate.provider, statusCode, sentBytes, chunks, acc,
			fmt.Sprintf("upstream read error: %v", readErr))
		s.handleFailoverFailure(st, candidate, failureKindStream, "stream", statusCode)
		return
	}
}

func recordStreamAbort(st *snapshot, selected *provider, statusCode int, sentBytes, chunks int64, acc *dataChunk, reason string) {
	providerName := st.redactProviderSecrets(selected.Name)
	redactedReason := st.redactProviderSecrets(reason)
	if st.payloadLoggingEnabled() {
		st.logAt(config.LogLevelInfo, "STREAM-ABORT provider=%s status=%d sent_bytes=%d chunks=%d reason=%s tail=%q",
			providerName, statusCode, sentBytes, chunks, redactedReason, redactedTail(st, acc))
		return
	}
	st.logAt(config.LogLevelWarn, "STREAM-ABORT provider=%s status=%d sent_bytes=%d chunks=%d reason=%s",
		providerName, statusCode, sentBytes, chunks, redactedReason)
}

func tail(acc *dataChunk) string {
	if acc == nil {
		return ""
	}
	return string(acc.buf)
}

func redactedTail(st *snapshot, acc *dataChunk) string {
	value := st.redactProviderSecrets(tail(acc))
	if len(value) > streamAbortBuf {
		value = value[len(value)-streamAbortBuf:]
	}
	return value
}

func isStreamingResponse(response *http.Response, payload map[string]json.RawMessage) bool {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false
	}

	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/event-stream") {
		return true
	}

	streamValue, ok := payload["stream"]
	if !ok {
		return false
	}
	var requested bool
	return json.Unmarshal(streamValue, &requested) == nil && requested
}

// markCooldown benches a Provider (scopeProvider) or one of its upstream
// models (scopeModel) for the configured Cooldown.
func (s *Server) markCooldown(st *snapshot, candidate route, scope cooldownScope, failure *failureRecord) {
	key := cooldownKey{provider: candidate.provider.id}
	if scope == scopeModel {
		key.model = candidate.upstream
	}
	if failure != nil {
		failure.scope = scope
	}
	s.cooldowns.mark(key, st.cooldownDuration, failure)
	st.logAt(config.LogLevelInfo, "%s - cooldown - %s", st.label(key), st.cooldownDuration)
}

func (s *Server) handleFailoverFailure(st *snapshot, candidate route, kind, category string, statusCode int) {
	name := st.redactProviderSecrets(candidate.provider.Name)
	if statusCode > 0 {
		st.logAt(config.LogLevelWarn, "%s - failover - %s - %d", name, category, statusCode)
	} else {
		st.logAt(config.LogLevelWarn, "%s - failover - %s", name, category)
	}
	s.markCooldown(st, candidate, scopeFor(kind, statusCode), &failureRecord{kind: kind, category: category, status: statusCode})
}

func (st *snapshot) logCancellation(phase string) {
	st.logAt(config.LogLevelInfo, "request canceled - %s", phase)
}

func (s *Server) logProviderResponse(st *snapshot, selected *provider, statusCode int, errorMessage string) {
	name := st.redactProviderSecrets(selected.Name)
	if statusCode == http.StatusOK && errorMessage == "" {
		// Successful response status is mandatory operational output at every log level.
		st.logger.Printf("%s status=%d", name, statusCode)
		return
	}
	if statusCode > 0 {
		if errorMessage != "" && st.payloadLoggingEnabled() {
			st.logAt(config.LogLevelInfo, "%s error - status=%d - %s", name, statusCode, st.sanitizeLogMessage(errorMessage))
			return
		}
		st.logAt(config.LogLevelError, "%s error - status=%d", name, statusCode)
		return
	}
	if errorMessage == "" {
		st.logAt(config.LogLevelError, "%s error", name)
		return
	}
	st.logAt(config.LogLevelError, "%s error - %s", name, st.sanitizeLogMessage(errorMessage))
}

func readErrorBodyForLog(body io.ReadCloser) ([]byte, error) {
	timer := time.AfterFunc(errorBodyReadTimeout, func() {
		_ = body.Close()
	})
	defer timer.Stop()

	responseBody, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > maxErrorBodyBytes {
		return nil, fmt.Errorf("upstream error response exceeds %d bytes", maxErrorBodyBytes)
	}
	return responseBody, nil
}

func errorMessageFromResponse(body []byte, statusCode int) string {
	var payload struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if len(payload.Error) > 0 && string(payload.Error) != "null" {
			var errorText string
			if json.Unmarshal(payload.Error, &errorText) == nil && strings.TrimSpace(errorText) != "" {
				return errorText
			}

			var structuredError struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(payload.Error, &structuredError) == nil && strings.TrimSpace(structuredError.Message) != "" {
				return structuredError.Message
			}
		}
		if strings.TrimSpace(payload.Message) != "" {
			return payload.Message
		}
		if strings.TrimSpace(payload.Detail) != "" {
			return payload.Detail
		}
	}

	return fmt.Sprintf("upstream returned HTTP %d", statusCode)
}

func sanitizeErrorMessage(message string) string {
	message = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		default:
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > maxLoggedErrorMessageLength {
		message = message[:maxLoggedErrorMessageLength] + "..."
	}
	return message
}

func isResponseHeaderTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutError interface{ Timeout() bool }
	return errors.As(err, &timeoutError) && timeoutError.Timeout()
}

func isFailoverStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusPaymentRequired ||
		(statusCode >= http.StatusInternalServerError && statusCode <= 599)
}

// isReasoningEffortUnsupportedError detects the provider-side rejection of an
// unsupported reasoning_effort value. Providers answer with HTTP 400 and a
// body like {"error":{"message":"reasoning_effort: unsupported value: ..."}},
// which is a request-shaping problem the proxy can fix by failing over to a
// provider that accepts the configured value.
//
// Both the extracted message and the raw body are scanned: extraction fails on
// bodies the JSON parser rejects (BOM prefix, trailing garbage, unfamiliar
// envelopes), while the distinctive substrings survive verbatim in the raw
// bytes the client ends up seeing.
func isReasoningEffortUnsupportedError(statusCode int, errorMessage string, responseBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	return containsUnsupportedReasoningEffort(errorMessage) ||
		containsUnsupportedReasoningEffort(string(responseBody))
}

func containsUnsupportedReasoningEffort(message string) bool {
	lowered := strings.ToLower(message)
	return strings.Contains(lowered, "reasoning_effort") && strings.Contains(lowered, "unsupported value")
}

func chatCompletionsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("base_url must be a valid URL: %w", err)
	}
	path := strings.TrimRight(parsed.Path, "/")
	parsed.Path = path + "/chat/completions"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func decodeRequestBody(body []byte) (map[string]json.RawMessage, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, fmt.Errorf("decode request body")
	}
	return payload, nil
}

func applyUpstreamOverrides(payload map[string]json.RawMessage, modelAlias string, reasoningEffort *config.ReasoningEffort) ([]byte, error) {
	forwardedPayload := make(map[string]json.RawMessage, len(payload)+2)
	for key, value := range payload {
		forwardedPayload[key] = value
	}
	encodedModelAlias, err := json.Marshal(modelAlias)
	if err != nil {
		return nil, fmt.Errorf("encode model alias: %w", err)
	}
	forwardedPayload["model"] = encodedModelAlias
	if reasoningEffort != nil {
		encodedEffort, err := json.Marshal(*reasoningEffort)
		if err != nil {
			return nil, fmt.Errorf("encode reasoning effort: %w", err)
		}
		forwardedPayload["reasoning_effort"] = encodedEffort
	} else {
		delete(forwardedPayload, "reasoning_effort")
	}
	return json.Marshal(forwardedPayload)
}

// setProviderHeaders exposes which upstream actually served the request so
// clients can attribute usage per provider/model. Set after copyHeaders so an
// upstream cannot spoof the values with its own headers.
func setProviderHeaders(destination http.Header, providerName, upstreamModel string) {
	destination.Set(ProviderHeader, providerName)
	destination.Set(ModelHeader, upstreamModel)
}

func copyHeaders(destination, source http.Header) {
	hopByHop := map[string]struct{}{
		"connection":          {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"transfer-encoding":   {},
		"upgrade":             {},
	}
	for _, connectionHeader := range source.Values("Connection") {
		for _, token := range strings.Split(connectionHeader, ",") {
			hopByHop[strings.ToLower(strings.TrimSpace(token))] = struct{}{}
		}
	}

	for key, values := range source {
		if _, excluded := hopByHop[strings.ToLower(key)]; excluded {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}
