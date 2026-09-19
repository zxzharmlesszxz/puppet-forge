package store

import (
	"context"
	"fmt"
	"time"
)

func (s *PostgresStore) RecordReleaseConsumer(ctx context.Context, observation ReleaseConsumerObservation) error {
	_, err := s.pool.Exec(ctx, `
		insert into release_consumers (
			consumer_team, consumer_name, consumer_role, owner, name, version, first_seen_at, last_seen_at, request_count
		) values ($1, $2, $3, $4, $5, $6, $7, $7, 1)
		on conflict (consumer_team, consumer_name, consumer_role, owner, name, version) do update set
			first_seen_at = least(release_consumers.first_seen_at, excluded.first_seen_at),
			last_seen_at = greatest(release_consumers.last_seen_at, excluded.last_seen_at),
			request_count = release_consumers.request_count + 1
	`, observation.ConsumerTeam, observation.ConsumerName, observation.ConsumerRole, observation.Owner, observation.Name, observation.Version, observation.ObservedAt.UTC())
	if err != nil {
		return fmt.Errorf("record release consumer: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListReleaseConsumers(ctx context.Context, since time.Time, limit int) ([]ReleaseConsumer, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `select count(*) from release_consumers where last_seen_at >= $1`, since.UTC()).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count release consumers: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		select c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version,
			coalesce(m.latest_version, ''), c.first_seen_at, c.last_seen_at, c.request_count
		from release_consumers c
		left join modules m on m.owner = c.owner and m.name = c.name
		where c.last_seen_at >= $1
		order by c.last_seen_at desc, c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version
		limit $2
	`, since.UTC(), limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list release consumers: %w", err)
	}
	defer rows.Close()
	consumers := make([]ReleaseConsumer, 0, min(total, limit))
	for rows.Next() {
		var consumer ReleaseConsumer
		if err := rows.Scan(&consumer.ConsumerTeam, &consumer.ConsumerName, &consumer.ConsumerRole, &consumer.Owner, &consumer.Name, &consumer.Version, &consumer.LatestVersion, &consumer.FirstSeenAt, &consumer.LastSeenAt, &consumer.Observations); err != nil {
			return nil, 0, fmt.Errorf("scan release consumer: %w", err)
		}
		consumers = append(consumers, consumer)
	}
	return consumers, total, rows.Err()
}

func (s *PostgresStore) ListLegacyReleaseConsumers(ctx context.Context, consumerTeam, query string, limit, offset int) ([]ReleaseConsumer, int, error) {
	const filter = `
		m.latest_version is not null and m.latest_version <> '' and c.version <> m.latest_version
		and ($1 = '' or c.consumer_team = $1)
		and ($2 = '' or position(lower($2) in lower(concat_ws(' ', c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version, m.latest_version))) > 0)
	`
	var total int
	if err := s.pool.QueryRow(ctx, `select count(*) from release_consumers c join modules m on m.owner = c.owner and m.name = c.name where `+filter, consumerTeam, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count legacy release consumers: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		select c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version,
			m.latest_version, c.first_seen_at, c.last_seen_at, c.request_count
		from release_consumers c
		join modules m on m.owner = c.owner and m.name = c.name
		where `+filter+`
		order by c.last_seen_at desc, c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version
		limit $3 offset $4
	`, consumerTeam, query, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list legacy release consumers: %w", err)
	}
	defer rows.Close()
	consumers := make([]ReleaseConsumer, 0, min(total, limit))
	for rows.Next() {
		var consumer ReleaseConsumer
		if err := rows.Scan(&consumer.ConsumerTeam, &consumer.ConsumerName, &consumer.ConsumerRole, &consumer.Owner, &consumer.Name, &consumer.Version, &consumer.LatestVersion, &consumer.FirstSeenAt, &consumer.LastSeenAt, &consumer.Observations); err != nil {
			return nil, 0, fmt.Errorf("scan legacy release consumer: %w", err)
		}
		consumers = append(consumers, consumer)
	}
	return consumers, total, rows.Err()
}

func (s *PostgresStore) PurgeReleaseConsumers(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `delete from release_consumers where last_seen_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("purge release consumers: %w", err)
	}
	return result.RowsAffected(), nil
}

func (s *SQLiteStore) RecordReleaseConsumer(ctx context.Context, observation ReleaseConsumerObservation) error {
	_, err := s.db.ExecContext(ctx, `
		insert into release_consumers (
			consumer_team, consumer_name, consumer_role, owner, name, version, first_seen_at, last_seen_at, request_count
		) values (?, ?, ?, ?, ?, ?, ?, ?, 1)
		on conflict (consumer_team, consumer_name, consumer_role, owner, name, version) do update set
			first_seen_at = min(release_consumers.first_seen_at, excluded.first_seen_at),
			last_seen_at = max(release_consumers.last_seen_at, excluded.last_seen_at),
			request_count = release_consumers.request_count + 1
	`, observation.ConsumerTeam, observation.ConsumerName, observation.ConsumerRole, observation.Owner, observation.Name, observation.Version, sqliteTime(observation.ObservedAt.UTC()), sqliteTime(observation.ObservedAt.UTC()))
	if err != nil {
		return fmt.Errorf("record release consumer: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListReleaseConsumers(ctx context.Context, since time.Time, limit int) ([]ReleaseConsumer, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `select count(*) from release_consumers where julianday(last_seen_at) >= julianday(?)`, sqliteTime(since.UTC())).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count release consumers: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		select c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version,
			coalesce(m.latest_version, ''), c.first_seen_at, c.last_seen_at, c.request_count
		from release_consumers c
		left join modules m on m.owner = c.owner and m.name = c.name
		where julianday(c.last_seen_at) >= julianday(?)
		order by c.last_seen_at desc, c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version
		limit ?
	`, sqliteTime(since.UTC()), limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list release consumers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	consumers := make([]ReleaseConsumer, 0, min(total, limit))
	for rows.Next() {
		var consumer ReleaseConsumer
		var firstSeenAt, lastSeenAt sqliteTimestamp
		if err := rows.Scan(&consumer.ConsumerTeam, &consumer.ConsumerName, &consumer.ConsumerRole, &consumer.Owner, &consumer.Name, &consumer.Version, &consumer.LatestVersion, &firstSeenAt, &lastSeenAt, &consumer.Observations); err != nil {
			return nil, 0, fmt.Errorf("scan release consumer: %w", err)
		}
		consumer.FirstSeenAt = firstSeenAt.Time
		consumer.LastSeenAt = lastSeenAt.Time
		consumers = append(consumers, consumer)
	}
	return consumers, total, rows.Err()
}

func (s *SQLiteStore) ListLegacyReleaseConsumers(ctx context.Context, consumerTeam, query string, limit, offset int) ([]ReleaseConsumer, int, error) {
	const filter = `
		m.latest_version is not null and m.latest_version <> '' and c.version <> m.latest_version
		and (? = '' or c.consumer_team = ?)
		and (? = '' or instr(lower(c.consumer_team || ' ' || c.consumer_name || ' ' || c.consumer_role || ' ' || c.owner || ' ' || c.name || ' ' || c.version || ' ' || m.latest_version), lower(?)) > 0)
	`
	var total int
	if err := s.db.QueryRowContext(ctx, `select count(*) from release_consumers c join modules m on m.owner = c.owner and m.name = c.name where `+filter, consumerTeam, consumerTeam, query, query).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count legacy release consumers: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		select c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version,
			m.latest_version, c.first_seen_at, c.last_seen_at, c.request_count
		from release_consumers c
		join modules m on m.owner = c.owner and m.name = c.name
		where `+filter+`
		order by c.last_seen_at desc, c.consumer_team, c.consumer_name, c.consumer_role, c.owner, c.name, c.version
		limit ? offset ?
	`, consumerTeam, consumerTeam, query, query, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list legacy release consumers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	consumers := make([]ReleaseConsumer, 0, min(total, limit))
	for rows.Next() {
		var consumer ReleaseConsumer
		var firstSeenAt, lastSeenAt sqliteTimestamp
		if err := rows.Scan(&consumer.ConsumerTeam, &consumer.ConsumerName, &consumer.ConsumerRole, &consumer.Owner, &consumer.Name, &consumer.Version, &consumer.LatestVersion, &firstSeenAt, &lastSeenAt, &consumer.Observations); err != nil {
			return nil, 0, fmt.Errorf("scan legacy release consumer: %w", err)
		}
		consumer.FirstSeenAt = firstSeenAt.Time
		consumer.LastSeenAt = lastSeenAt.Time
		consumers = append(consumers, consumer)
	}
	return consumers, total, rows.Err()
}

func (s *SQLiteStore) PurgeReleaseConsumers(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `delete from release_consumers where julianday(last_seen_at) < julianday(?)`, sqliteTime(before.UTC()))
	if err != nil {
		return 0, fmt.Errorf("purge release consumers: %w", err)
	}
	return result.RowsAffected()
}
