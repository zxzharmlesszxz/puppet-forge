package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"golang.org/x/mod/semver"
)

type ReleasePageStore interface {
	ListReleasesPageFiltered(context.Context, string, string, int, int) ([]domain.Release, int, error)
}

const releasePageSelect = `
	select r.id, r.module_id, m.owner, m.name, coalesce(r.source, 'local'), r.version,
		coalesce(r.description, ''), coalesce(r.readme, ''), r.file_name, r.content_type,
		r.size_bytes, r.md5, r.sha256, r.storage_path, coalesce(r.upstream_slug, ''),
		coalesce(r.upstream_file_uri, ''), r.metadata, r.created_at
	from releases r join modules m on m.id = r.module_id
`

// Encode numeric identifiers by length and value; release versions sort above
// prereleases, numeric prerelease identifiers below text. Build metadata is ignored.
func releaseVersionOrder(version string) string {
	version = strings.TrimSpace(version)
	canonical := semver.Canonical("v" + strings.TrimPrefix(version, "v"))
	if canonical == "" {
		return "0" + version
	}
	core, pre, _ := strings.Cut(strings.TrimPrefix(canonical, "v"), "-")
	var key strings.Builder
	key.WriteByte('1')
	// strings.Builder writes cannot fail, including through fmt.Fprintf.
	for part := range strings.SplitSeq(core, ".") {
		_, _ = fmt.Fprintf(&key, "%010d%s!", len(part), part)
	}
	if pre == "" {
		key.WriteByte('1')
	} else {
		key.WriteByte('0')
		for part := range strings.SplitSeq(pre, ".") {
			numeric := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) == -1
			if numeric {
				_, _ = fmt.Fprintf(&key, "0%010d%s!", len(part), part)
			} else {
				key.WriteString("1" + part + "!")
			}
		}
	}
	return key.String()
}

type releaseOrderUpdate struct{ id, key string }

func scanReleaseOrderUpdates(rows rowScanner) ([]releaseOrderUpdate, error) {
	var updates []releaseOrderUpdate
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, err
		}
		updates = append(updates, releaseOrderUpdate{id, releaseVersionOrder(version)})
	}
	return updates, rows.Err()
}

func (s *PostgresStore) ListReleasesPageFiltered(ctx context.Context, module, version string, limit, offset int) ([]domain.Release, int, error) {
	if err := validateModulePagination(limit, offset); err != nil {
		return nil, 0, err
	}
	const filter = ` where ($1 = '' or m.slug = $1) and ($2 = '' or r.version = $2)`
	var total int
	if err := s.pool.QueryRow(ctx, `select count(*) from releases r join modules m on m.id = r.module_id`+filter, module, version).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count filtered releases: %w", err)
	}
	rows, err := s.pool.Query(ctx, releasePageSelect+filter+` order by r.version_order desc, r.created_at desc, r.id limit $3 offset $4`, module, version, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list release page: %w", err)
	}
	defer rows.Close()
	releases := make([]domain.Release, 0, min(total, limit))
	for rows.Next() {
		release, err := scanPostgresRelease(rows, "scan release page")
		if err != nil {
			return nil, 0, err
		}
		releases = append(releases, release)
	}
	return releases, total, rows.Err()
}

func (s *SQLiteStore) ListReleasesPageFiltered(ctx context.Context, module, version string, limit, offset int) ([]domain.Release, int, error) {
	if err := validateModulePagination(limit, offset); err != nil {
		return nil, 0, err
	}
	const filter = ` where (?1 = '' or m.slug = ?1) and (?2 = '' or r.version = ?2)`
	var total int
	if err := s.db.QueryRowContext(ctx, `select count(*) from releases r join modules m on m.id = r.module_id`+filter, module, version).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count filtered releases: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, releasePageSelect+filter+` order by r.version_order desc, r.created_at desc, r.id limit ?3 offset ?4`, module, version, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list release page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	releases := make([]domain.Release, 0, min(total, limit))
	for rows.Next() {
		release, err := scanRelease(rows)
		if err != nil {
			return nil, 0, err
		}
		releases = append(releases, release)
	}
	return releases, total, rows.Err()
}
