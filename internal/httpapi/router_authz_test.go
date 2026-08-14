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

func TestCurrentAuthorizerUsesStaleSnapshotWhileRefreshIsInProgress(t *testing.T) {
	t.Parallel()

	baseStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(baseStore.Close)
	blockingStore := &blockingAccessConfigStore{
		SQLiteStore: baseStore,
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
	initial, err := auth.NewAuthorizer([]auth.TeamConfig{{Team: "teamname", ReadTokens: []string{"read-token"}}})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	router := &Router{
		modules:             service.NewModuleService(blockingStore, nil, "modules", nil),
		authorizer:          initial,
		authorizerRefreshed: time.Now().Add(-2 * accessConfigRefreshTTL),
		refreshAccessConfig: true,
	}
	firstDone := make(chan authorizerResult, 1)
	go func() {
		authorizer, err := router.currentAuthorizer(context.Background())
		firstDone <- authorizerResult{authorizer: authorizer, err: err}
	}()
	<-blockingStore.started

	secondDone := make(chan authorizerResult, 1)
	go func() {
		authorizer, err := router.currentAuthorizer(context.Background())
		secondDone <- authorizerResult{authorizer: authorizer, err: err}
	}()
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

	baseStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(baseStore.Close)
	blockingStore := &blockingAccessConfigStore{
		SQLiteStore: baseStore,
		started:     make(chan struct{}, 1),
		release:     make(chan struct{}),
	}
	initial, err := auth.NewAuthorizer([]auth.TeamConfig{{Team: "teamname", ReadTokens: []string{"read-token"}}})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	router := &Router{
		modules:             service.NewModuleService(blockingStore, nil, "modules", nil),
		authorizer:          initial,
		authorizerRefreshed: time.Now().Add(-accessConfigMaxStaleTTL - time.Second),
		refreshAccessConfig: true,
	}
	firstDone := make(chan authorizerResult, 1)
	go func() {
		authorizer, err := router.currentAuthorizer(context.Background())
		firstDone <- authorizerResult{authorizer: authorizer, err: err}
	}()
	<-blockingStore.started

	got, err := router.currentAuthorizer(context.Background())
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

func TestCurrentAuthorizerRejectsExpiredSnapshotAfterRefreshFailure(t *testing.T) {
	t.Parallel()

	baseStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(baseStore.Close)
	initial, err := auth.NewAuthorizer([]auth.TeamConfig{{Team: "teamname", PublishTokens: []string{"publish-token"}}})
	if err != nil {
		t.Fatalf("NewAuthorizer() error = %v", err)
	}
	router := &Router{
		modules:             service.NewModuleService(&failingAccessConfigStore{SQLiteStore: baseStore, err: errors.New("database unavailable")}, nil, "modules", nil),
		authorizer:          initial,
		authorizerRefreshed: time.Now().Add(-accessConfigMaxStaleTTL - time.Second),
		refreshAccessConfig: true,
	}

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

	baseStore, err := store.NewSQLiteStore("sqlite://:memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(baseStore.Close)
	router := &Router{
		modules:             service.NewModuleService(&failingAccessConfigStore{SQLiteStore: baseStore, err: errors.New("database unavailable")}, nil, "modules", nil),
		refreshAccessConfig: true,
	}

	if err := router.refreshManageAuthorizer(context.Background()); err == nil {
		t.Fatal("refreshManageAuthorizer() error = nil, want fail-closed error")
	}
}
