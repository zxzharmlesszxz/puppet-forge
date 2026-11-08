package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type replacingDownloadStorage struct {
	fixedDownloadStorage
	replace func()
	opened  []string
}

func (s *replacingDownloadStorage) Stat(ctx context.Context, objectPath string) (storage.ObjectAttrs, error) {
	attrs, err := s.fixedDownloadStorage.Stat(ctx, objectPath)
	s.replace()
	return attrs, err
}

func (s *replacingDownloadStorage) Open(ctx context.Context, objectPath string) (storage.ObjectReader, error) {
	s.opened = append(s.opened, objectPath)
	if objectPath != "old-object" {
		return storage.ObjectReader{}, errors.New("opened replacement object")
	}
	return s.fixedDownloadStorage.Open(ctx, objectPath)
}

func (s *replacingDownloadStorage) OpenRange(ctx context.Context, objectPath string, offset, length int64) (storage.ObjectReader, error) {
	s.opened = append(s.opened, objectPath)
	if objectPath != "old-object" {
		return storage.ObjectReader{}, errors.New("opened replacement object")
	}
	return s.fixedDownloadStorage.OpenRange(ctx, objectPath, offset, length)
}

func TestReleaseArchiveResponseKeepsSnapshotDuringReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, method, rangeHeader, match, body string
		status                                 int
	}{
		{name: "full", method: http.MethodGet, body: "original archive", status: http.StatusOK},
		{name: "range", method: http.MethodGet, rangeHeader: "bytes=0-2", body: "ori", status: http.StatusPartialContent},
		{name: "head", method: http.MethodHead, status: http.StatusOK},
		{name: "conditional", method: http.MethodGet, match: `"old-sha"`, status: http.StatusNotModified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.NewSQLiteStore("sqlite://" + t.TempDir() + "/snapshot.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(st.Close)
			module, err := st.UpsertModule(t.Context(), "teamname", "demo")
			if err != nil {
				t.Fatal(err)
			}
			release := store.NewRelease(module.ID, "teamname", "demo", "1.0.0", "", "", "demo.tar.gz", "application/gzip", "old-md5", "old-sha", "old-object", 16, nil)
			release, err = st.CreateRelease(t.Context(), release)
			if err != nil {
				t.Fatal(err)
			}
			artifacts := &replacingDownloadStorage{body: []byte("original archive"), contentType: "application/gzip"}
			artifacts.replace = func() {
				replacement := release
				replacement.StoragePath, replacement.SHA256, replacement.SizeBytes = "new-object", "new-sha", 99
				if _, err := st.CreateRelease(t.Context(), replacement); err != nil {
					t.Fatal(err)
				}
			}
			router := &Router{modules: service.NewModuleService(st, artifacts, "modules", nil)}
			req := httptest.NewRequest(tc.method, "/archive", nil)
			req.Header.Set("Range", tc.rangeHeader)
			req.Header.Set("If-None-Match", tc.match)
			rec := httptest.NewRecorder()
			router.writeReleaseArchive(rec, req, release)
			if rec.Code != tc.status || rec.Body.String() != tc.body || rec.Header().Get("ETag") != `"old-sha"` {
				t.Fatalf("mixed snapshot response: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
			}
			if tc.status == http.StatusOK && rec.Header().Get("Content-Length") != fmt.Sprint(release.SizeBytes) {
				t.Fatal("wrong snapshot size")
			}
			if tc.body != "" && len(artifacts.opened) != 1 {
				t.Fatalf("opened paths = %v", artifacts.opened)
			}
			stored, err := st.GetRelease(t.Context(), release.Owner, release.Name, release.Version)
			if err != nil || stored.StoragePath != "new-object" {
				t.Fatalf("replacement was not exercised: %#v %v", stored, err)
			}
		})
	}
}

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
		{name: "moderation forbidden", err: service.ErrModerationForbidden, status: http.StatusForbidden, code: "forbidden"},
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
