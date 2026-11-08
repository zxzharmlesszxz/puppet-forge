package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestDeleteAuthorizedChecksOriginAndTeam(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"local", "upstream", "mixed"} {
		for _, role := range []string{"global", "team", "other", "publisher"} {
			for _, version := range []string{"", "1.0.0"} {
				for _, force := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/module=%v/force=%v", source, role, version == "", force), func(t *testing.T) {
						t.Parallel()
						st, err := store.NewSQLiteStore("sqlite://:memory:")
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(st.Close)
						module, err := st.UpsertModule(t.Context(), "teamname", "demo")
						if err != nil {
							t.Fatal(err)
						}
						for index, releaseVersion := range []string{"1.0.0", "2.0.0"} {
							releaseSource := source
							if source == "mixed" {
								releaseSource = "local"
								if index == 1 {
									releaseSource = "upstream"
								}
							}
							release := store.NewRelease(module.ID, "teamname", "demo", releaseVersion, "", "", "demo.tar.gz", "application/gzip", "md5", "sha", "modules/"+releaseVersion, 1, nil)
							release.Source = releaseSource
							if _, err := st.CreateRelease(t.Context(), release); err != nil {
								t.Fatal(err)
							}
						}
						principal := auth.Principal{Team: "teamname", CanRead: true}
						switch role {
						case "global":
							principal.CanAdmin = true
						case "team":
							principal.CanManageTeam = true
						case "other":
							principal.CanManageTeam = true
							principal.ManagedTeams = map[string]struct{}{"other": {}}
						case "publisher":
							principal.CanPublish = true
						}
						svc := NewModuleService(st, &testArtifactStorage{}, "modules", nil)
						err = svc.DeleteAuthorized(t.Context(), principal, "teamname", "demo", version, force, time.Now().Add(-time.Hour))
						allowed := role == "global" || role == "team" && source == "local"
						if !allowed {
							if !errors.Is(err, ErrDeleteForbidden) {
								t.Fatalf("delete error = %v, want ErrDeleteForbidden", err)
							}
							for _, original := range []string{"1.0.0", "2.0.0"} {
								if _, err := st.GetRelease(t.Context(), "teamname", "demo", original); err != nil {
									t.Fatalf("denied delete changed release: %v", err)
								}
							}
							if pending, err := st.CountArtifactDeletions(t.Context()); err != nil || pending != 0 {
								t.Fatalf("denied delete queued artifacts: pending=%d err=%v", pending, err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						if _, err := st.GetRelease(t.Context(), "teamname", "demo", "1.0.0"); !errors.Is(err, store.ErrNotFound) {
							t.Fatalf("authorized delete left target release: %v", err)
						}
						if version != "" {
							if _, err := st.GetRelease(t.Context(), "teamname", "demo", "2.0.0"); err != nil {
								t.Fatalf("release delete removed another version: %v", err)
							}
						}
					})
				}
			}
		}
	}
}

type upstreamOnDeleteLockStore struct {
	*store.SQLiteStore
	release domain.Release
}

func (s *upstreamOnDeleteLockStore) LockModule(ctx context.Context, owner, name string) (store.ModuleUnlock, error) {
	unlock, err := s.SQLiteStore.LockModule(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	s.release.Source = "upstream"
	// The competing import belongs to another request, not the caller's audit.
	importCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.CreateRelease(importCtx, s.release); err != nil {
		_ = unlock()
		return nil, err
	}
	return unlock, nil
}

func TestDeleteAuthorizedRechecksOriginUnderLock(t *testing.T) {
	t.Parallel()
	svc, st, release := newReviewService(t)
	principal := reviewAdmin()
	if allowed, err := svc.CanDeleteModule(t.Context(), principal, "teamname", "demo"); err != nil || !allowed {
		t.Fatalf("initial local permission = %v, %v", allowed, err)
	}
	svc = NewModuleService(&upstreamOnDeleteLockStore{SQLiteStore: st, release: release}, &testArtifactStorage{}, "modules", nil)
	if err := svc.DeleteAuthorized(t.Context(), principal, "teamname", "demo", release.Version, true, time.Now()); !errors.Is(err, ErrDeleteForbidden) {
		t.Fatalf("changed origin bypassed authorization: %v", err)
	}
	if _, err := st.GetRelease(t.Context(), "teamname", "demo", release.Version); err != nil {
		t.Fatal(err)
	}
}
