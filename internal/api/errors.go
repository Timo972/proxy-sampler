package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/timo972/proxy-sampler/internal/api/openapi"
)

const (
	errorCodeInvalidRequest        = "invalid_request"
	errorCodeNotFound              = "not_found"
	errorCodeSessionNotRunning     = "session_not_running"
	errorCodeInternal              = "internal_error"
	errorCodeDependencyUnavailable = "dependency_unavailable"
)

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.code }

func invalidRequest() error {
	return &apiError{status: http.StatusBadRequest, code: errorCodeInvalidRequest, message: "request is invalid"}
}

func notFound() error {
	return &apiError{status: http.StatusNotFound, code: errorCodeNotFound, message: "session not found"}
}

func sessionNotRunning() error {
	return &apiError{status: http.StatusConflict, code: errorCodeSessionNotRunning, message: "session is not running"}
}

func internalError() error {
	return &apiError{status: http.StatusInternalServerError, code: errorCodeInternal, message: "internal server error"}
}

func dependencyUnavailable() error {
	return &apiError{status: http.StatusServiceUnavailable, code: errorCodeDependencyUnavailable, message: "dependency unavailable"}
}

func requestErrorHandler(w http.ResponseWriter, _ *http.Request, _ error) {
	writeError(w, invalidRequest())
}

func responseErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, err)
}

func writeError(w http.ResponseWriter, err error) {
	value := &apiError{}
	if !errors.As(err, &value) {
		value = internalError().(*apiError)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(value.status)
	_ = json.NewEncoder(w).Encode(openapi.Error{Code: value.code, Message: value.message})
}
