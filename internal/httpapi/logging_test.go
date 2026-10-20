package httpapi

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

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func captureHTTPLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

type failingManageSessionBackend struct {
	manageSessionBackend
	err error
}

func (b failingManageSessionBackend) GetManageSession(context.Context, string, time.Time) (store.ManageSession, error) {
	return store.ManageSession{}, b.err
}

func TestManageSessionLogsOnlyUnexpectedStoreErrors(t *testing.T) {
	logs := captureHTTPLogs(t)
	ctx := observability.ContextWithRequestID(t.Context(), "session-request")
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("lookup: %w", store.ErrNotFound), false},
		{errors.New("database or disk is full"), true},
	} {
		logs.Reset()
		sessions := newManageSessionStore(failingManageSessionBackend{err: tc.err}, "session-secret")
		if _, ok := sessions.Session(ctx, "private-session-cookie", time.Now()); ok {
			t.Fatal("failed session lookup authenticated the request")
		}
		if tc.want {
			for _, want := range []string{"read manage session failed", "session-request", "database or disk is full"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("session log misses %q: %s", want, logs.String())
				}
			}
		} else if logs.Len() != 0 {
			t.Errorf("expected missing session to be silent: %s", logs.String())
		}
		if strings.Contains(logs.String(), "private-session-cookie") {
			t.Fatal("session cookie leaked into logs")
		}
	}
}

func TestManageMutationErrorsHaveSafeMessagesAndOneDiagnostic(t *testing.T) {
	logs := captureHTTPLogs(t)
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"database", errors.New("database or disk is full /private/internal.db"), http.StatusInternalServerError, "internal server error"},
		{"upstream", fmt.Errorf("%w: private upstream detail", service.ErrUpstreamHydration), http.StatusBadGateway, "upstream service is unavailable"},
		{"validation", fmt.Errorf("%w: invalid archive", service.ErrValidation), http.StatusBadRequest, "invalid archive"},
		{"conflict", store.ErrConflict, http.StatusConflict, store.ErrConflict.Error()},
	} {
		for _, async := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/async=%t", tc.name, async), func(t *testing.T) {
				logs.Reset()
				req := httptest.NewRequest(http.MethodPost, "/manage/modules", strings.NewReader("token=private-form-token"))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if async {
					req.Header.Set("X-Puppet-Forge-Async-Mutation", "true")
				}
				response := httptest.NewRecorder()
				response.Header().Set(requestIDHeader, "mutation-request")
				respondManageMutationServiceError(response, req, tc.err)
				message := response.Body.String()
				if async {
					if response.Code != tc.status {
						t.Errorf("status=%d, want %d", response.Code, tc.status)
					}
				} else {
					if response.Code != http.StatusFound {
						t.Errorf("status=%d, want redirect", response.Code)
					}
					target, err := url.Parse(response.Header().Get("Location"))
					if err != nil {
						t.Fatal(err)
					}
					message = target.Query().Get("error")
				}
				if !strings.Contains(message, tc.message) {
					t.Errorf("message=%q, want %q", message, tc.message)
				}
				wantLogs := 0
				if tc.status >= http.StatusInternalServerError {
					wantLogs = 1
					if !strings.Contains(logs.String(), tc.err.Error()) || !strings.Contains(logs.String(), "mutation-request") {
						t.Errorf("server log misses the cause or request ID: %s", logs.String())
					}
					if !strings.Contains(message, "mutation-request") || strings.Contains(message, "private upstream detail") || strings.Contains(message, "/private/internal.db") {
						t.Errorf("unsafe or uncorrelated response: %s", message)
					}
				}
				if got := strings.Count(logs.String(), `"msg":"http request failed"`); got != wantLogs {
					t.Errorf("diagnostic count=%d, want %d: %s", got, wantLogs, logs.String())
				}
				if strings.Contains(logs.String(), "private-form-token") {
					t.Fatal("submitted credential leaked into logs")
				}
			})
		}
	}
}

type failingManageConfigStore struct {
	store.Store
}

func (failingManageConfigStore) LoadTeamConfigs(context.Context) ([]auth.TeamConfig, error) {
	return nil, errors.New("database or disk is full")
}

func TestManageFormsDistinguishBackendAndValidationErrors(t *testing.T) {
	t.Parallel()
	router := &Router{modules: service.NewModuleService(failingManageConfigStore{}, nil, "modules", nil)}
	for _, build := range []func(*http.Request) ([]auth.TeamConfig, string, error){
		router.accessConfigsWithGlobalAdmins,
		router.accessConfigsWithoutTeam,
		router.publishSpaceConfigsFromForm,
		func(req *http.Request) ([]auth.TeamConfig, string, error) {
			return router.accessConfigsWithUpsertedTeam(req, auth.Principal{CanAdmin: true})
		},
	} {
		req := formRequest(url.Values{"team": {"teamname"}, "space": {"shared"}})
		_, _, err := build(req)
		if !errors.Is(err, errManageBackend) || manageFormErrorStatus(err) != http.StatusInternalServerError {
			t.Errorf("backend error was not classified as internal: %v", err)
		}
	}
	if status := manageFormErrorStatus(errors.New("team is required")); status != http.StatusBadRequest {
		t.Errorf("validation status=%d, want %d", status, http.StatusBadRequest)
	}
}
