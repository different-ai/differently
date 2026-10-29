package api

import (
	"errors"
	"fmt"
	"net/http"
)

// Error codes. Clients should branch on Code, never on Message.
const (
	CodeInvalidRequest        = "invalid_request"
	CodeUnauthenticated       = "unauthenticated"
	CodePermissionDenied      = "permission_denied"
	CodeNotFound              = "not_found"
	CodeConflict              = "conflict"
	CodeQuotaExceeded         = "quota_exceeded"
	CodeNoEligiblePool        = "no_eligible_pool"
	CodeUnsupportedCapability = "unsupported_capability"
	CodeUnsupportedOperation  = "unsupported_operation"
	CodeProviderUnavailable   = "provider_unavailable"
	CodeInvalidState          = "invalid_state"
	CodeRateLimited           = "rate_limited"
	CodeSuspended             = "suspended"
	CodeInternal              = "internal"
)

// Error is the structured error returned by every endpoint.
type Error struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Details   map[string]string `json:"details,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
}

// ErrorEnvelope wraps Error on the wire: {"error": {...}}.
type ErrorEnvelope struct {
	Error *Error `json:"error"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// HTTPStatus maps an error code to its HTTP status.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied, CodeSuspended:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeInvalidState:
		return http.StatusConflict
	case CodeQuotaExceeded, CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeNoEligiblePool, CodeUnsupportedCapability:
		return http.StatusUnprocessableEntity
	case CodeUnsupportedOperation:
		return http.StatusNotImplemented
	case CodeProviderUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// With returns a copy of e with an extra detail.
func (e *Error) With(key, value string) *Error {
	c := *e
	c.Details = map[string]string{}
	for k, v := range e.Details {
		c.Details[k] = v
	}
	c.Details[key] = value
	return &c
}

// Errorf builds an Error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError extracts an *Error from err, wrapping unknown errors as internal.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// IsCode reports whether err carries the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
