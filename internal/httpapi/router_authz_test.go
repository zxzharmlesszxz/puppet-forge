package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type blockingAccessConfigStore struct {
	*store.SQLiteStore
	started chan struct{}
	release chan struct{}
}

type failingAccessConfigStore struct {
	*store.SQLiteStore
	err error
}

func (s *failingAccessConfigStore) LoadTeamConfigs(context.Context) ([]auth.TeamConfig, error) {
	return nil, s.err
}

type authorizerResult struct {
	authorizer *auth.Authorizer
	err        error
}

func newSQLiteStoreForTests(t *testing.T) *store.SQLiteStore {
	t.Helper()
	baseStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(baseStore.Close)
	return baseStore
}

func blockingStoreRouter(ctx *testing.T) *blockingAccessConfigStore {
	baseStore := newHTTPAPITestStore(ctx)
	return &blockingAccessConfigStore{
		SQLiteStore: baseStore,
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
}

func mustNewAuthorizer(t *testing.T, team, role, token string) *auth.Authorizer {
	t.Helper()
	var cfg []auth.TeamConfig
	switch role {
	case "read":
		cfg = []auth.TeamConfig{{Team: team, ReadTokens: []string{token}}}
	case "publish":
		cfg = []auth.TeamConfig{{Team: team, PublishTokens: []string{token}}}
	default:
		t.Fatalf("unsupported role %q", role)
	}
	authorizer, err := auth.NewAuthorizer(cfg)
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	return authorizer
}

func makeAuthorizerRouter(baseStore store.ModuleStore, initial *auth.Authorizer, refreshedAgo time.Duration) *Router {
	return &Router{
		modules:             service.NewModuleService(baseStore, nil, "modules", nil),
		authorizer:          initial,
		authorizerRefreshed: time.Now().Add(refreshedAgo),
		refreshAccessConfig: true,
	}
}

func failingAccessStoreRouter(ctx *testing.T, initial *auth.Authorizer, refreshedAgo time.Duration) *Router {
	baseStore := newSQLiteStoreForTests(ctx)
	return &Router{
		modules:             service.NewModuleService(&failingAccessConfigStore{SQLiteStore: baseStore, err: errors.New("database unavailable")}, nil, "modules", nil),
		authorizer:          initial,
		authorizerRefreshed: time.Now().Add(refreshedAgo),
		refreshAccessConfig: true,
	}
}

func (s *blockingAccessConfigStore) LoadTeamConfigs(ctx context.Context) ([]auth.TeamConfig, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return s.SQLiteStore.LoadTeamConfigs(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func asyncCurrentAuthorizer(router *Router) <-chan authorizerResult {
	done := make(chan authorizerResult, 1)
	go func() {
		authorizer, err := router.currentAuthorizer(context.Background())
		done <- authorizerResult{authorizer: authorizer, err: err}
	}()
	return done
}

func TestCurrentAuthorizerUsesStaleSnapshotWhileRefreshIsInProgress(t *testing.T) {
	t.Parallel()

	blockingStore := blockingStoreRouter(t)
	initial := mustNewAuthorizer(t, "teamname", "read", "read-token")
	router := makeAuthorizerRouter(blockingStore, initial, -2*accessConfigRefreshTTL)
	firstDone := asyncCurrentAuthorizer(router)
	<-blockingStore.started

	secondDone := asyncCurrentAuthorizer(router)
	select {
	case result := <-secondDone:
		if result.err != nil {
			t.Fatalf("currentAuthorizer() error = %v", result.err)
		}
		if result.authorizer != initial {
			t.Fatalf("currentAuthorizer() = %p, want stale snapshot %p", result.authorizer, initial)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("currentAuthorizer() blocked behind access-config refresh")
	}

	close(blockingStore.release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("access-config refresh did not finish")
	}
}

func TestCurrentAuthorizerRejectsExpiredSnapshotWhileRefreshIsInProgress(t *testing.T) {
	t.Parallel()

	blockingStore := blockingStoreRouter(t)
	initial := mustNewAuthorizer(t, "teamname", "read", "read-token")
	router := makeAuthorizerRouter(blockingStore, initial, -accessConfigMaxStaleTTL-time.Second)
	firstDone := asyncCurrentAuthorizer(router)
	<-blockingStore.started

	waitCtx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	got, err := router.currentAuthorizer(waitCtx)
	if err == nil {
		t.Fatalf("currentAuthorizer() = %p, nil; want unavailable error", got)
	}
	if got != nil {
		t.Fatalf("currentAuthorizer() = %p, want nil after stale deadline", got)
	}

	close(blockingStore.release)
	select {
	case result := <-firstDone:
		if result.err != nil {
			t.Fatalf("refreshing currentAuthorizer() error = %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("access-config refresh did not finish")
	}
}

func TestCurrentAuthorizerWaitsForUsableSnapshot(t *testing.T) {
	t.Parallel()
	for _, cold := range []bool{true, false} {
		name := "expired"
		if cold {
			name = "cold"
		}
		t.Run(name, func(t *testing.T) {
			blockingStore := blockingStoreRouter(t)
			if err := blockingStore.ReplaceTeamConfigs(t.Context(), []auth.TeamConfig{{Team: "consumer", ReadTokens: []string{"new-read-token"}}}); err != nil {
				t.Fatal(err)
			}
			router := makeAuthorizerRouter(blockingStore, nil, -accessConfigMaxStaleTTL-time.Second)
			router.tokenHasher = httpAPITestTokenHasher()
			if cold {
				router.authorizerRefreshed = time.Time{}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			t.Cleanup(func() { close(blockingStore.release) })
			firstDone := asyncCurrentAuthorizer(router)
			select {
			case <-blockingStore.started:
			case <-ctx.Done():
				t.Fatal("access-config refresh did not start")
			}
			secondDone := make(chan authorizerResult, 1)
			go func() {
				authorizer, err := router.currentAuthorizer(ctx)
				secondDone <- authorizerResult{authorizer: authorizer, err: err}
			}()
			select {
			case result := <-secondDone:
				t.Fatalf("request did not wait for a usable snapshot: %v", result.err)
			case <-time.After(25 * time.Millisecond):
			}
			blockingStore.release <- struct{}{}
			for _, done := range []<-chan authorizerResult{firstDone, secondDone} {
				select {
				case result := <-done:
					if result.err != nil || result.authorizer == nil {
						t.Fatalf("currentAuthorizer() = %v, %v", result.authorizer, result.err)
					}
					if _, ok := result.authorizer.AuthenticateToken("new-read-token"); !ok {
						t.Fatal("refreshed snapshot rejected the read token")
					}
				case <-ctx.Done():
					t.Fatal("request did not finish after access-config refresh")
				}
			}
		})
	}
}

func TestCurrentAuthorizerRejectsExpiredSnapshotAfterRefreshFailure(t *testing.T) {
	t.Parallel()

	initial := mustNewAuthorizer(t, "teamname", "publish", "publish-token")
	router := failingAccessStoreRouter(t, initial, -accessConfigMaxStaleTTL-time.Second)

	got, err := router.currentAuthorizer(context.Background())
	if err == nil {
		t.Fatalf("currentAuthorizer() = %p, nil; want unavailable error", got)
	}
	if got != nil {
		t.Fatalf("currentAuthorizer() = %p, want nil after stale deadline", got)
	}
}

func TestRefreshManageAuthorizerFailsClosed(t *testing.T) {
	t.Parallel()

	baseStore := newSQLiteStoreForTests(t)
	router := &Router{
		modules:             service.NewModuleService(&failingAccessConfigStore{SQLiteStore: baseStore, err: errors.New("database unavailable")}, nil, "modules", nil),
		refreshAccessConfig: true,
	}

	if err := router.refreshManageAuthorizer(context.Background()); err == nil {
		t.Fatal("refreshManageAuthorizer() error = nil, want fail-closed error")
	}
}
