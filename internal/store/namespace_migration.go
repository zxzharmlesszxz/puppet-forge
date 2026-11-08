package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func migrateSQLiteNamespaces(tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRow(`select exists(select 1 from sqlite_master where type='table' and name='access_publish_owners')`).Scan(&exists); err != nil {
		return fmt.Errorf("check legacy namespace table: %w", err)
	}
	if !exists {
		return nil
	}
	rows, err := tx.Query(namespaceMigrationNamesQuery)
	if err != nil {
		return fmt.Errorf("read namespace migration names: %w", err)
	}
	err = validateNamespaceMigrationNames(rows)
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if _, err := tx.Exec(`insert into access_teams (team) select distinct owner from access_publish_owners where true on conflict (team) do nothing`); err != nil {
		return fmt.Errorf("migrate namespaces to teams: %w", err)
	}
	if _, err := tx.Exec(`drop table access_publish_owners`); err != nil {
		return fmt.Errorf("drop legacy namespace table: %w", err)
	}
	return nil
}

func migratePostgresNamespaces(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin namespace migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	if err := tx.QueryRow(ctx, `select to_regclass('access_publish_owners') is not null`).Scan(&exists); err != nil {
		return fmt.Errorf("check legacy namespace table: %w", err)
	}
	if exists {
		rows, err := tx.Query(ctx, namespaceMigrationNamesQuery)
		if err != nil {
			return fmt.Errorf("read namespace migration names: %w", err)
		}
		err = validateNamespaceMigrationNames(rows)
		rows.Close()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into access_teams (team) select distinct owner from access_publish_owners on conflict (team) do nothing`); err != nil {
			return fmt.Errorf("migrate namespaces to teams: %w", err)
		}
		if _, err := tx.Exec(ctx, `drop table access_publish_owners`); err != nil {
			return fmt.Errorf("drop legacy namespace table: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit namespace migration: %w", err)
	}
	return nil
}

const namespaceMigrationNamesQuery = `select team from access_teams union select owner from access_publish_owners order by team`

func validateNamespaceMigrationNames(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) error {
	names := make(map[string]string)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan namespace migration name: %w", err)
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if previous, exists := names[key]; exists {
			return fmt.Errorf("namespace migration conflict: %q and %q differ only by letter case; resolve legacy assignments before retrying", previous, name)
		}
		names[key] = name
	}
	return rows.Err()
}
