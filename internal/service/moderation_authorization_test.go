package service

import (
	"errors"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func TestModerationUnblocksDeletedResourcesWithoutRepublishing(t *testing.T) {
	for _, version := range []string{"", "1.0.0"} {
		t.Run("version="+version, func(t *testing.T) {
			svc, st, release := newReviewService(t)
			if err := svc.Moderate(t.Context(), reviewAdmin(), release.Owner, release.Name, version, "block", "broken", "", release.SHA256); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteModule(t.Context(), release.Owner, release.Name); err != nil {
				t.Fatal(err)
			}
			if err := svc.RequireAvailable(t.Context(), release.Owner, release.Name, version); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("delete bypassed block: %v", err)
			}
			if err := svc.Moderate(t.Context(), reviewAdmin(), release.Owner, release.Name, version, "unblock", "fixed", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := svc.RequireAvailable(t.Context(), release.Owner, release.Name, version); err != nil {
				t.Fatalf("resource remained blocked: %v", err)
			}
			if _, err := st.GetRelease(t.Context(), release.Owner, release.Name, release.Version); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unblock recreated archive metadata: %v", err)
			}
		})
	}
}

func TestModerationRechecksOriginUnderLock(t *testing.T) {
	for _, version := range []string{"", "1.0.0"} {
		t.Run("version="+version, func(t *testing.T) {
			svc, st, release := newReviewService(t)
			if allowed, err := svc.CanDeleteModule(t.Context(), reviewAdmin(), release.Owner, release.Name); err != nil || !allowed {
				t.Fatalf("initial permission = %v %v", allowed, err)
			}
			svc = NewModuleService(&upstreamOnDeleteLockStore{SQLiteStore: st, release: release}, &testArtifactStorage{}, "modules", nil)
			ctx := store.ContextWithMutationAudit(t.Context(), store.AuditEvent{Actor: "reviewer", Action: "moderate_resource", Result: "success", OccurredAt: time.Now()})
			if err := svc.Moderate(ctx, reviewAdmin(), release.Owner, release.Name, version, "block", "broken", "", release.SHA256); !errors.Is(err, ErrModerationForbidden) {
				t.Fatalf("changed origin = %v", err)
			}
			status, err := st.GetResourceStatus(t.Context(), release.Owner, release.Name, version)
			if err != nil || status.State == domain.StateBlocked {
				t.Fatalf("denied mutation changed state: %#v %v", status, err)
			}
			_, count, err := st.ListAuditEvents(t.Context(), nil, "", 10, 0)
			if err != nil || count != 0 {
				t.Fatalf("denied mutation recorded success: %d %v", count, err)
			}
			if store.MutationAuditRecorded(ctx) {
				t.Fatal("competing import consumed the denied moderation audit")
			}
			if err := svc.Moderate(t.Context(), auth.Principal{CanAdmin: true}, release.Owner, release.Name, version, "block", "broken", "", release.SHA256); err != nil {
				t.Fatalf("global moderation = %v", err)
			}
		})
	}
}

func TestModerationRejectsNonAdministrators(t *testing.T) {
	svc, _, release := newReviewService(t)
	for _, principal := range []auth.Principal{{CanRead: true}, {CanPublish: true, Team: "teamname"}, {CanManageTeam: true, Team: "other"}} {
		if err := svc.Moderate(t.Context(), principal, release.Owner, release.Name, "", "block", "broken", "", ""); !errors.Is(err, ErrModerationForbidden) {
			t.Fatalf("unauthorized moderation = %v", err)
		}
	}
}
