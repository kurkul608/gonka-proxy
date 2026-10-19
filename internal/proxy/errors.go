package proxy

import (
	"encoding/json"
	"net/http"
)

// Error types and codes the proxy itself emits, in the OpenAI error shape
// {"error":{"message":...,"type":...,"param":...,"code":...}}.
const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeServer         = "server_error"

	CodeModelNotFound          = "model_not_found"
	CodeNoProviderAvailable    = "no_provider_available"
	CodeInvalidAPIKey          = "invalid_api_key"
	CodeInvalidReasoningEffort = "invalid_reasoning_effort"
	CodeInvalidMaxRoutingTime  = "invalid_max_routing_time"
	CodeInvalidRequestBody     = "invalid_request_body"
	CodeMethodNotAllowed       = "method_not_allowed"
	CodeUpstreamRequestFailed  = "upstream_request_failed"
	CodeRequestTooLarge        = "request_too_large"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// WriteError writes an OpenAI-style JSON error response.
func WriteError(w http.ResponseWriter, status int, errorType, code, message string) {
	writeErrorParam(w, status, errorType, code, "", message)
}

func writeErrorParam(w http.ResponseWriter, status int, errorType, code, param, message string) {
	detail := errorDetail{Message: message, Type: errorType, Code: code}
	if param != "" {
		detail.Param = &param
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: detail})
}

func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	WriteError(w, http.StatusMethodNotAllowed, ErrorTypeInvalidRequest, CodeMethodNotAllowed, "method not allowed")
}
