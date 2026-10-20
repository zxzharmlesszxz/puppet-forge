package webauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"golang.org/x/oauth2"
)

func captureOIDCLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestOIDCProtocolDiagnosticsExcludeCredentials(t *testing.T) {
	logs := captureOIDCLogs(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=private-code&state=private-state", nil)
	req = req.WithContext(observability.ContextWithRequestID(req.Context(), "oidc-request"))
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&oauth2.RetrieveError{Response: &http.Response{StatusCode: 401}, ErrorCode: "invalid_client", ErrorDescription: "private-provider-description", Body: []byte("private-provider-body")}, `"provider_error":"invalid_client"`},
		{&oauth2.RetrieveError{ErrorCode: "private-provider-code"}, `"provider_error":"unrecognized_error"`},
		{&url.Error{Op: "Post", URL: "https://user:private-password@idp.test/token?code=private-code", Err: errors.New("private-network-detail")}, `"error_type":"*url.Error"`},
		{errors.New("private-jwt-contents"), `"error_type":"*errors.errorString"`},
		{fmt.Errorf("exchange: %w", context.DeadlineExceeded), `"reason":"deadline_exceeded"`},
		{context.Canceled, `"reason":"context_canceled"`},
	} {
		logs.Reset()
		logOIDCProtocolFailure(req, "exchange_code", tc.err)
		for _, want := range []string{"oidc-request", `"operation":"exchange_code"`, tc.want} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("protocol log misses %q: %s", want, logs.String())
			}
		}
		if strings.Contains(logs.String(), "private-") {
			t.Errorf("protocol log exposes private data: %s", logs.String())
		}
	}
}

type failingOIDCSessions struct {
	store.OIDCSessionStore
	err error
}

func (s failingOIDCSessions) GetOIDCSession(context.Context, string, time.Time) (store.OIDCSession, error) {
	return store.OIDCSession{}, s.err
}

type failingOIDCStates struct {
	store.OIDCStateStore
}

func (failingOIDCStates) CreateOIDCState(context.Context, store.OIDCState) error {
	return errors.New("database or disk is full")
}

func TestOIDCStorageDiagnostics(t *testing.T) {
	logs := captureOIDCLogs(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	req = req.WithContext(observability.ContextWithRequestID(req.Context(), "oidc-storage-request"))
	req.AddCookie(&http.Cookie{Name: "session", Value: "private-session-cookie"})
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("lookup: %w", store.ErrNotFound), false},
		{errors.New("database or disk is full"), true},
	} {
		logs.Reset()
		auth := &OIDCAuth{sessionName: "session", sessionStore: failingOIDCSessions{err: tc.err}}
		if _, ok := auth.Session(req); ok {
			t.Fatal("failed OIDC session lookup authenticated the request")
		}
		if tc.want {
			for _, want := range []string{"oidc-storage-request", "read_session", "database or disk is full"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("session log misses %q: %s", want, logs.String())
				}
			}
		} else if logs.Len() != 0 {
			t.Errorf("expected expired or missing session to be silent: %s", logs.String())
		}
		if strings.Contains(logs.String(), "private-session-cookie") {
			t.Fatal("session cookie leaked into logs")
		}
	}
	logs.Reset()
	auth := &OIDCAuth{stateStore: failingOIDCStates{}}
	response := httptest.NewRecorder()
	auth.Login(response, req)
	if response.Code != http.StatusInternalServerError || response.Header().Get("Location") != "" {
		t.Fatalf("state persistence failure returned %d with redirect %q", response.Code, response.Header().Get("Location"))
	}
	for _, want := range []string{"oidc-storage-request", "create_state", "database or disk is full"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("state log misses %q: %s", want, logs.String())
		}
	}
	if strings.Contains(response.Body.String(), "database or disk is full") {
		t.Fatal("internal database error leaked to the browser")
	}
}
