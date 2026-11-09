package store

import "testing"

func migrationTestOperations(t *testing.T, st ModuleStore) (execute func(string) error, migrate func() error) {
	t.Helper()
	ctx := t.Context()
	switch backend := st.(type) {
	case *SQLiteStore:
		return func(query string) error {
			_, err := backend.db.ExecContext(ctx, query)
			return err
		}, func() error { return migrateSQLiteSchema(backend.db, backend.tokenHasher) }
	case *PostgresStore:
		return func(query string) error {
			_, err := backend.pool.Exec(ctx, query)
			return err
		}, func() error { return backend.ensureOperationalTables(ctx) }
	default:
		t.Fatal("unsupported migration backend")
		return nil, nil
	}
}
