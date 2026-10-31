package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type mutationAuditKey struct{}

type releaseMutationWriter interface {
	currentLatestVersion(moduleID string) (string, error)
	updateLatestVersion(moduleID, latest string) error
	commit() error
}

func finishReleaseMutation(release domain.Release, writer releaseMutationWriter) error {
	current, err := writer.currentLatestVersion(release.ModuleID)
	if err != nil {
		return err
	}
	if err := writer.updateLatestVersion(release.ModuleID, latestVersionWithCandidate(current, release.Version)); err != nil {
		return fmt.Errorf("update module latest version: %w", err)
	}
	if err := writer.commit(); err != nil {
		return fmt.Errorf("commit release tx: %w", err)
	}
	return nil
}

type mutationAudit struct {
	mu       sync.Mutex
	event    AuditEvent
	recorded bool
}

func ContextWithMutationAudit(ctx context.Context, event AuditEvent) context.Context {
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	event.Attributes = maps.Clone(event.Attributes)
	return context.WithValue(ctx, mutationAuditKey{}, &mutationAudit{event: event})
}

func EnrichMutationAudit(ctx context.Context, attrs map[string]string) {
	if audit, ok := ctx.Value(mutationAuditKey{}).(*mutationAudit); ok {
		audit.mu.Lock()
		defer audit.mu.Unlock()
		if audit.event.Attributes == nil {
			audit.event.Attributes = make(map[string]string)
		}
		maps.Copy(audit.event.Attributes, attrs)
	}
}

func PendingMutationAudit(ctx context.Context) (AuditEvent, bool) {
	audit, ok := ctx.Value(mutationAuditKey{}).(*mutationAudit)
	if !ok {
		return AuditEvent{}, false
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	event := audit.event
	event.Attributes = maps.Clone(event.Attributes)
	return event, !audit.recorded
}

func MutationAuditRecorded(ctx context.Context) bool {
	audit, ok := ctx.Value(mutationAuditKey{}).(*mutationAudit)
	if !ok {
		return false
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	return audit.recorded
}

func MarkMutationAuditRecorded(ctx context.Context) {
	if audit, ok := ctx.Value(mutationAuditKey{}).(*mutationAudit); ok {
		audit.mu.Lock()
		audit.recorded = true
		audit.mu.Unlock()
	}
}

func commitPostgresMutation(ctx context.Context, tx pgx.Tx) error {
	if event, pending := PendingMutationAudit(ctx); pending {
		if err := recordPostgresAuditEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	MarkMutationAuditRecorded(ctx)
	return nil
}

func commitSQLiteMutation(ctx context.Context, tx *sql.Tx) error {
	if event, pending := PendingMutationAudit(ctx); pending {
		if err := recordSQLiteAuditEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	MarkMutationAuditRecorded(ctx)
	return nil
}
