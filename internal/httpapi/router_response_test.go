package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestWriteErrorHidesInternalCauseAndReturnsRequestID(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	recorder.Header().Set(requestIDHeader, "request-1234")
	writeError(recorder, http.StatusInternalServerError, errors.New("sql failed for /secret/path"))

	var response map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"] != "internal server error" || response["code"] != "internal_error" {
		t.Fatalf("unexpected safe error response: %#v", response)
	}
	if response["request_id"] != "request-1234" {
		t.Fatalf("request_id = %q", response["request_id"])
	}
}

func TestWriteServiceErrorMapsKnownErrorCategories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "validation", err: service.ErrValidation, status: http.StatusBadRequest, code: "validation_error"},
		{name: "store not found", err: store.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
		{name: "object not found", err: storage.ErrObjectNotFound, status: http.StatusNotFound, code: "not_found"},
		{name: "conflict", err: store.ErrConflict, status: http.StatusConflict, code: "conflict"},
		{name: "protected", err: service.ErrProtectedDelete, status: http.StatusConflict, code: "protected_resource"},
		{name: "upstream", err: service.ErrUpstreamHydration, status: http.StatusBadGateway, code: "upstream_unavailable"},
		{name: "unexpected", err: errors.New("database unavailable"), status: http.StatusInternalServerError, code: "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			writeServiceError(recorder, tt.err)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			var response map[string]string
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response["code"] != tt.code {
				t.Fatalf("code = %q, want %q", response["code"], tt.code)
			}
		})
	}
}

func TestWriteErrorUsesProtectedResourceTaxonomy(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeError(recorder, http.StatusConflict, service.ErrProtectedDelete)

	var response map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["code"] != "protected_resource" {
		t.Fatalf("code = %q, want protected_resource", response["code"])
	}
}
