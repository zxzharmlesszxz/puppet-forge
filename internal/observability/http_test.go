package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
		"/metrics":                        "other",
		"/auth/callback":                  "/auth/*",
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

func TestResponseRecorderExposesUnderlyingResponseControllerCapabilities(t *testing.T) {
	t.Parallel()

	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush() error = %v", err)
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

func TestMiddlewareLogLevelsAndPanicStack(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range []struct {
		path    string
		status  int
		minimum slog.Level
		want    string
		panics  bool
	}{
		{"/healthz", http.StatusOK, slog.LevelInfo, "", false},
		{"/readyz", http.StatusOK, slog.LevelInfo, "", false},
		{"/readyz", http.StatusServiceUnavailable, slog.LevelInfo, "INFO", false},
		{"/healthz", http.StatusOK, slog.LevelDebug, "DEBUG", false},
		{"/manage", http.StatusOK, slog.LevelInfo, "INFO", false},
		{"/manage", http.StatusInternalServerError, slog.LevelInfo, "INFO", true},
	} {
		output.Reset()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: tc.minimum})))
		handler := NewMiddleware().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.panics {
				panic("test-panic")
			}
			w.WriteHeader(tc.status)
		}))
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req = req.WithContext(ContextWithRequestID(req.Context(), "logging-request"))
		handler.ServeHTTP(httptest.NewRecorder(), req)
		completionLevel := ""
		foundStack := false
		for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err)
			}
			if record["request_id"] != "logging-request" {
				t.Errorf("log misses request ID: %s", line)
			}
			if record["msg"] == "http request" {
				completionLevel, _ = record["level"].(string)
			}
			if record["msg"] == "http handler panic" {
				stack, _ := record["stack"].(string)
				foundStack = strings.Contains(stack, "runtime/debug.Stack") && record["level"] == "ERROR"
			}
		}
		if completionLevel != tc.want {
			t.Errorf("%s status %d: level=%q, want %q", tc.path, tc.status, completionLevel, tc.want)
		}
		if tc.panics && !foundStack {
			t.Error("panic diagnostic misses stack trace")
		}
	}
}
