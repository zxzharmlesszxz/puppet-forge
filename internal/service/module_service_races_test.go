package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type pausedModuleLockStore struct {
	*store.SQLiteStore
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (s *pausedModuleLockStore) LockModule(ctx context.Context, owner, name string) (store.ModuleUnlock, error) {
	s.once.Do(func() {
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
		}
	})
	return s.SQLiteStore.LockModule(ctx, owner, name)
}

func TestReleaseEnrichmentPreservesConcurrentMutation(t *testing.T) {
	for _, operation := range []string{"hydrate", "checksum"} {
		for _, mutation := range []string{"replace", "delete"} {
			t.Run(operation+"/"+mutation, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				st, err := store.NewSQLiteStore("sqlite://:memory:")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(st.Close)
				module, err := st.UpsertModule(ctx, "puppetlabs", "stdlib")
				if err != nil {
					t.Fatal(err)
				}
				old, err := st.CreateRelease(ctx, domain.Release{
					ID: "old", ModuleID: module.ID, Owner: module.Owner, Name: module.Name,
					Version: "1.0.0", Source: "upstream", FileName: "archive.tar.gz", ContentType: "application/gzip", StoragePath: "upstream-cache/old.tar.gz",
				})
				if err != nil {
					t.Fatal(err)
				}
				paused := &pausedModuleLockStore{SQLiteStore: st, entered: make(chan struct{}), resume: make(chan struct{})}
				var resumeOnce sync.Once
				resume := func() { resumeOnce.Do(func() { close(paused.resume) }) }
				t.Cleanup(resume)
				artifacts := &testArtifactStorage{}
				if err := artifacts.Upload(ctx, old.StoragePath, "application/gzip", []byte("old artifact")); err != nil {
					t.Fatal(err)
				}
				svc := newUpstreamModuleService(t, paused, artifacts, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, `{"version":"1.0.0","file_uri":"/v3/files/puppetlabs-stdlib-1.0.0.tar.gz"}`)
				}))
				type outcome struct {
					release domain.Release
					err     error
				}
				done := make(chan outcome, 1)
				go func() {
					var result outcome
					if operation == "hydrate" {
						result.release, result.err = svc.GetRelease(ctx, old.Owner, old.Name, old.Version)
					} else {
						result.release, result.err = svc.calculateReleaseChecksums(ctx, old)
					}
					done <- result
				}()
				select {
				case <-paused.entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				unlock, err := st.LockModule(ctx, old.Owner, old.Name)
				if err != nil {
					t.Fatal(err)
				}
				if mutation == "replace" {
					replacement := old
					replacement.Source, replacement.StoragePath, replacement.SHA256 = "local", "modules/new.tar.gz", "new-checksum"
					_, err = st.CreateRelease(ctx, replacement)
				} else {
					err = st.DeleteRelease(ctx, old.Owner, old.Name, old.Version)
				}
				unlockErr := unlock()
				if err := errors.Join(err, unlockErr); err != nil {
					t.Fatal(err)
				}
				resume()
				result := <-done
				stored, getErr := st.GetRelease(ctx, old.Owner, old.Name, old.Version)
				if mutation == "delete" {
					if !errors.Is(result.err, store.ErrNotFound) || !errors.Is(getErr, store.ErrNotFound) {
						t.Fatalf("deleted release resurrected: result=%#v, stored=%#v, err=%v", result, stored, getErr)
					}
				} else if result.err != nil || getErr != nil || stored.Source != "local" || stored.StoragePath != "modules/new.tar.gz" || stored.SHA256 != "new-checksum" || result.release.Source != "local" {
					t.Fatalf("replacement overwritten: result=%#v, stored=%#v, err=%v", result, stored, getErr)
				}
				if pending, err := st.CountArtifactDeletions(ctx); err != nil || pending != 0 {
					t.Fatalf("replacement queued for deletion: pending=%d err=%v", pending, err)
				}
			})
		}
	}
}
