package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *PostgresStore) RecordAuditEvent(ctx context.Context, event AuditEvent) error {
	attributes, err := json.Marshal(event.Attributes)
	if err != nil {
		return fmt.Errorf("encode audit event attributes: %w", err)
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	_, err = s.pool.Exec(ctx, `
		insert into audit_events (
			id, occurred_at, request_id, actor, auth_method, action, result, reason, source_ip, attributes_json
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, event.ID, event.OccurredAt.UTC(), event.RequestID, event.Actor, event.AuthMethod, event.Action, event.Result, event.Reason, event.SourceIP, attributes)
	if err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListAuditEvents(ctx context.Context, scopes []string, query string, limit, offset int) ([]AuditEvent, int, error) {
	if scopes == nil {
		scopes = []string{}
	}
	const filter = `
		(cardinality($1::text[]) = 0
			or attributes_json->>'team' = any($1::text[])
			or attributes_json->>'new_team' = any($1::text[])
			or attributes_json->>'space' = any($1::text[])
			or (action = 'oidc_login' and string_to_array(attributes_json->>'actor_teams', ',') && $1::text[])
			or split_part(attributes_json->>'module', '/', 1) = any($1::text[]))
		and ($2 = '' or position(lower($2) in lower(concat_ws(' ', request_id, actor, auth_method, action, result, reason, source_ip, attributes_json::text))) > 0)`
	var total int
	if err := s.pool.QueryRow(ctx, `select count(*) from audit_events where `+filter, scopes, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		select id, occurred_at, request_id, actor, auth_method, action, result, reason, source_ip, attributes_json
		from audit_events
		where `+filter+`
		order by occurred_at desc, id desc
		limit $3 offset $4
	`, scopes, query, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	events := make([]AuditEvent, 0, min(total, limit))
	for rows.Next() {
		var event AuditEvent
		var attributes []byte
		if err := rows.Scan(&event.ID, &event.OccurredAt, &event.RequestID, &event.Actor, &event.AuthMethod, &event.Action, &event.Result, &event.Reason, &event.SourceIP, &attributes); err != nil {
			return nil, 0, fmt.Errorf("scan audit event: %w", err)
		}
		if err := json.Unmarshal(attributes, &event.Attributes); err != nil {
			return nil, 0, fmt.Errorf("decode audit event attributes: %w", err)
		}
		events = append(events, event)
	}
	return events, total, rows.Err()
}

func (s *SQLiteStore) RecordAuditEvent(ctx context.Context, event AuditEvent) error {
	attributes, err := json.Marshal(event.Attributes)
	if err != nil {
		return fmt.Errorf("encode audit event attributes: %w", err)
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		insert into audit_events (
			id, occurred_at, request_id, actor, auth_method, action, result, reason, source_ip, attributes_json
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ID, sqliteTime(event.OccurredAt.UTC()), event.RequestID, event.Actor, event.AuthMethod, event.Action, event.Result, event.Reason, event.SourceIP, string(attributes))
	if err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListAuditEvents(ctx context.Context, scopes []string, query string, limit, offset int) ([]AuditEvent, int, error) {
	scopeJSON, err := json.Marshal(scopes)
	if err != nil {
		return nil, 0, fmt.Errorf("encode audit event scopes: %w", err)
	}
	const filter = `
		(?1 = 0 or exists (
			select 1 from json_each(?2) as scope
			where scope.value in (
				json_extract(attributes_json, '$.team'),
				json_extract(attributes_json, '$.new_team'),
				json_extract(attributes_json, '$.space'),
				substr(json_extract(attributes_json, '$.module'), 1, instr(json_extract(attributes_json, '$.module'), '/') - 1)
			)
			or (action = 'oidc_login' and instr(',' || coalesce(json_extract(attributes_json, '$.actor_teams'), '') || ',', ',' || scope.value || ',') > 0)
		))
		and (?3 = '' or instr(lower(request_id || ' ' || actor || ' ' || auth_method || ' ' || action || ' ' || result || ' ' || reason || ' ' || source_ip || ' ' || attributes_json), lower(?3)) > 0)`
	countArgs := []any{len(scopes), string(scopeJSON), query}
	var total int
	if err := s.db.QueryRowContext(ctx, `select count(*) from audit_events where `+filter, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", err)
	}
	listArgs := append(append([]any{}, countArgs...), limit, offset)
	rows, err := s.db.QueryContext(ctx, `
		select id, occurred_at, request_id, actor, auth_method, action, result, reason, source_ip, attributes_json
		from audit_events
		where `+filter+`
		order by occurred_at desc, id desc
		limit ?4 offset ?5
	`, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := make([]AuditEvent, 0, min(total, limit))
	for rows.Next() {
		var event AuditEvent
		var occurredAt sqliteTimestamp
		var attributes string
		if err := rows.Scan(&event.ID, &occurredAt, &event.RequestID, &event.Actor, &event.AuthMethod, &event.Action, &event.Result, &event.Reason, &event.SourceIP, &attributes); err != nil {
			return nil, 0, fmt.Errorf("scan audit event: %w", err)
		}
		event.OccurredAt = occurredAt.Time
		if err := json.Unmarshal([]byte(attributes), &event.Attributes); err != nil {
			return nil, 0, fmt.Errorf("decode audit event attributes: %w", err)
		}
		events = append(events, event)
	}
	return events, total, rows.Err()
}
