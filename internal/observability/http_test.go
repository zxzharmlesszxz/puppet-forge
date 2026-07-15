package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClassifyRoute(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/":                               "/",
		"/healthz":                        "/healthz",
		"/readyz":                         "/readyz",
		"/metrics":                        "/metrics",
		"/manage":                         "/manage",
		"/manage/login":                   "/manage/*",
		"/api/v1/modules":                 "/api/v1/modules",
		"/api/v1/modules/teamname/apache": "/api/v1/modules/*",
		"/modules/teamname/apache":        "/modules/*",
		"/v3/files/pkg.tar.gz":            "/v3/files/*",
		"/v3/modules/teamname-apache":     "/v3/*",
		"/unknown":                        "other",
	}

	for path, want := range cases {
		if got := classifyRoute(path); got != want {
			t.Fatalf("classifyRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestResponseRecorderTracksStatusAndBytes(t *testing.T) {
	t.Parallel()

	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusCreated)
	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != 5 || rec.bytes != 5 {
		t.Fatalf("unexpected byte counts: n=%d recorder=%d", n, rec.bytes)
	}
	if rec.status != http.StatusCreated {
		t.Fatalf("unexpected status: %d", rec.status)
	}
}

func TestMiddlewareRecordsRequests(t *testing.T) {
	middleware := NewMiddleware()
	beforeRequests := testutil.ToFloat64(middleware.requestsTotal.WithLabelValues(http.MethodPost, "/api/v1/modules", "201"))
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/modules", nil)
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", resp.Code, http.StatusCreated)
	}
	if got := testutil.ToFloat64(middleware.requestsTotal.WithLabelValues(http.MethodPost, "/api/v1/modules", "201")) - beforeRequests; got != 1 {
		t.Fatalf("request counter delta = %v, want 1", got)
	}
}

func TestMiddlewareRecordsPanicAsHTTP500(t *testing.T) {
	middleware := NewMiddleware()
	beforeRequests := testutil.ToFloat64(middleware.requestsTotal.WithLabelValues(http.MethodGet, "/manage/*", "500"))
	beforePanics := testutil.ToFloat64(middleware.panicsTotal.WithLabelValues(http.MethodGet, "/manage/*"))
	handler := middleware.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/manage/access", nil)
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d body=%s", resp.Code, http.StatusInternalServerError, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), http.StatusText(http.StatusInternalServerError)) {
		t.Fatalf("response body does not contain 500 text: %s", resp.Body.String())
	}
	if got := testutil.ToFloat64(middleware.requestsTotal.WithLabelValues(http.MethodGet, "/manage/*", "500")) - beforeRequests; got != 1 {
		t.Fatalf("request counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(middleware.panicsTotal.WithLabelValues(http.MethodGet, "/manage/*")) - beforePanics; got != 1 {
		t.Fatalf("panic counter delta = %v, want 1", got)
	}
}

func TestMiddlewareRecordsPanicAfterHeadersWithoutChangingStatus(t *testing.T) {
	middleware := NewMiddleware()
	beforeRequests := testutil.ToFloat64(middleware.requestsTotal.WithLabelValues(http.MethodGet, "/modules/*", "202"))
	beforePanics := testutil.ToFloat64(middleware.panicsTotal.WithLabelValues(http.MethodGet, "/modules/*"))
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/modules/teamname/apache", nil)
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusAccepted {
		t.Fatalf("response status = %d, want %d body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}
	if got := testutil.ToFloat64(middleware.requestsTotal.WithLabelValues(http.MethodGet, "/modules/*", "202")) - beforeRequests; got != 1 {
		t.Fatalf("request counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(middleware.panicsTotal.WithLabelValues(http.MethodGet, "/modules/*")) - beforePanics; got != 1 {
		t.Fatalf("panic counter delta = %v, want 1", got)
	}
}
