package store

import (
	"context"
	"errors"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type testReleaseMutationWriter struct {
	readErr, updateErr, commitErr error
	latest                        string
	committed                     bool
}

func (w *testReleaseMutationWriter) currentLatestVersion(string) (string, error) {
	return "2.0.0", w.readErr
}

func (w *testReleaseMutationWriter) updateLatestVersion(_ string, latest string) error {
	w.latest = latest
	return w.updateErr
}

func (w *testReleaseMutationWriter) commit() error {
	w.committed = true
	return w.commitErr
}

func TestFinishReleaseMutationPreservesOrderAndErrors(t *testing.T) {
	failure := errors.New("storage failure")
	for _, tc := range []struct {
		name       string
		candidate  string
		wantLatest string
		readErr    error
		updateErr  error
		commitErr  error
	}{
		{name: "advance", candidate: "3.0.0", wantLatest: "3.0.0"},
		{name: "keep latest", candidate: "1.0.0", wantLatest: "2.0.0"},
		{name: "read failure", candidate: "3.0.0", readErr: failure},
		{name: "update failure", candidate: "3.0.0", wantLatest: "3.0.0", updateErr: failure},
		{name: "commit failure", candidate: "3.0.0", wantLatest: "3.0.0", commitErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &testReleaseMutationWriter{readErr: tc.readErr, updateErr: tc.updateErr, commitErr: tc.commitErr}
			err := finishReleaseMutation(domain.Release{ModuleID: "module", Version: tc.candidate}, writer)
			if writer.latest != tc.wantLatest {
				t.Errorf("latest=%s, want %s", writer.latest, tc.wantLatest)
			}
			if writer.committed != (tc.readErr == nil && tc.updateErr == nil) {
				t.Errorf("commit called=%v after read/update error", writer.committed)
			}
			if want := errors.Join(tc.readErr, tc.updateErr, tc.commitErr); (err == nil) != (want == nil) || want != nil && !errors.Is(err, failure) {
				t.Fatalf("finalization error=%v, want %v", err, want)
			}
		})
	}
}

func TestStoreParityMutationAuditIsAtomic(t *testing.T) {
	for _, tc := range parityStoreCases(t) {
		for _, operation := range []string{"delete_module", "delete_release", "publish", "replace", "access"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				st := tc.open(t)
				t.Cleanup(st.Close)
				ctx := t.Context()
				configs := []auth.TeamConfig{{Team: "teamname", OIDCGroups: []string{"original"}}}
				if err := st.ReplaceTeamConfigs(ctx, configs); err != nil {
					t.Fatal(err)
				}
				module, err := st.UpsertModule(ctx, "teamname", "module")
				if err != nil {
					t.Fatal(err)
				}
				original, err := st.CreateRelease(ctx, NewRelease(module.ID, module.Owner, module.Name, "1.0.0", "original", "", "archive.tar.gz", "application/gzip", "", "sha", "modules/original", 1, nil))
				if err != nil {
					t.Fatal(err)
				}
				var injectionErr error
				switch backend := st.(type) {
				case *PostgresStore:
					_, injectionErr = backend.pool.Exec(ctx, `alter table audit_events add constraint reject_test_audit check (action <> 'rejected_mutation')`)
				case *SQLiteStore:
					_, injectionErr = backend.db.ExecContext(ctx, `create trigger reject_test_audit before insert on audit_events when new.action = 'rejected_mutation' begin select raise(abort, 'audit unavailable'); end`)
				}
				if injectionErr != nil {
					t.Fatal(injectionErr)
				}
				mutate := func(mutationCtx context.Context) error {
					switch operation {
					case "delete_module":
						return st.DeleteModule(mutationCtx, module.Owner, module.Name)
					case "delete_release":
						return st.DeleteRelease(mutationCtx, module.Owner, module.Name, original.Version)
					case "access":
						return st.ReplaceTeamConfigs(mutationCtx, []auth.TeamConfig{{Team: "teamname", OIDCGroups: []string{"changed"}}})
					default:
						release := original
						release.Description, release.StoragePath = "changed", "modules/changed"
						if operation == "publish" {
							release.ID, release.Version = "new-release", "2.0.0"
							_, err := st.CreateReleaseIfAbsent(mutationCtx, release)
							return err
						}
						_, err := st.CreateRelease(mutationCtx, release)
						return err
					}
				}
				event := AuditEvent{Actor: "test-admin", Action: "rejected_mutation", Result: "success", Attributes: map[string]string{"module": "teamname/module", "justification": "repair"}}
				failedCtx := ContextWithMutationAudit(ctx, event)
				if err := mutate(failedCtx); err == nil || MutationAuditRecorded(failedCtx) {
					t.Fatalf("mutation committed without audit: err=%v", err)
				}
				unchanged, err := st.GetRelease(ctx, module.Owner, module.Name, original.Version)
				if err != nil || unchanged.Description != "original" || unchanged.StoragePath != "modules/original" {
					t.Fatalf("failed mutation changed release: %#v, %v", unchanged, err)
				}
				if _, err := st.GetRelease(ctx, module.Owner, module.Name, "2.0.0"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("failed publish left a release: %v", err)
				}
				if pending, err := st.(ArtifactDeletionStore).CountArtifactDeletions(ctx); err != nil || pending != 0 {
					t.Fatalf("failed mutation queued artifacts: %d, %v", pending, err)
				}
				loaded, err := st.LoadTeamConfigs(ctx)
				if err != nil || len(loaded) != 1 || len(loaded[0].OIDCGroups) != 1 || loaded[0].OIDCGroups[0] != "original" {
					t.Fatalf("failed mutation changed access: %#v, %v", loaded, err)
				}
				event.Action = "allowed_mutation"
				mutationCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				successCtx := ContextWithMutationAudit(mutationCtx, event)
				if err := mutate(successCtx); err != nil {
					t.Fatal(err)
				}
				cancel()
				if !MutationAuditRecorded(successCtx) {
					t.Fatal("commit did not record audit")
				}
				events, total, err := st.ListAuditEvents(ctx, nil, "", 10, 0)
				if err != nil || total != 1 || len(events) != 1 || events[0].Actor != "test-admin" || events[0].Attributes["justification"] != "repair" {
					t.Fatalf("durable audit after cancellation = %#v total=%d err=%v", events, total, err)
				}
			})
		}
	}
}
