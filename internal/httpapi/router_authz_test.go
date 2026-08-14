package httpapi

import (
	"context"
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
		refreshAccessConfig: true,
	}
	firstDone := make(chan *auth.Authorizer, 1)
	go func() {
		firstDone <- router.currentAuthorizer(context.Background())
	}()
	<-blockingStore.started

	secondDone := make(chan *auth.Authorizer, 1)
	go func() {
		secondDone <- router.currentAuthorizer(context.Background())
	}()
	select {
	case got := <-secondDone:
		if got != initial {
			t.Fatalf("currentAuthorizer() = %p, want stale snapshot %p", got, initial)
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
