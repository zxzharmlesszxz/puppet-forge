package store

import (
	"context"
	"fmt"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type ResourceMetricStore interface {
	ListResourceMetricSummaries(context.Context) ([]domain.ResourceMetricSummary, error)
}

// Release states include parent restrictions; Deleted is not a persistent state
// after schema 11. Only existing catalog rows contribute to the inventory.
const resourceMetricsQuery = `
with resources as (
 select 'module' as kind,
 case when exists(select 1 from module_origins o where o.owner=m.owner and o.name=m.name and o.source='local') then 'local'
 when exists(select 1 from module_origins o where o.owner=m.owner and o.name=m.name and o.source='upstream') then 'upstream' else 'unknown' end as source,
 coalesce(nullif(s.state,''),'available') as state, coalesce(s.changed_at,'') as changed_at
 from modules m left join resource_states s on s.owner=m.owner and s.name=m.name and s.version=''
 union all
 select 'release', coalesce(nullif(r.source,''),'local'),
 case when ms.state='blocked' or rs.state='blocked' then 'blocked'
 when ms.state='pending' or rs.state='pending' then 'pending'
 when ms.state='deprecated' or rs.state='deprecated' then 'deprecated'
 when ms.state='deleted' or rs.state='deleted' then 'unknown' else 'available' end,
 case when ms.state='pending' then ms.changed_at else coalesce(rs.changed_at,'') end
 from releases r join modules m on m.id=r.module_id
 left join resource_states ms on ms.owner=m.owner and ms.name=m.name and ms.version=''
 left join resource_states rs on rs.owner=m.owner and rs.name=m.name and rs.version=r.version
)
select kind,source,state,count(*),min(case when state='pending' then substr(changed_at,1,19) else '' end)
from resources group by kind,source,state order by kind,source,state`

func (s *SQLiteStore) ListResourceMetricSummaries(ctx context.Context) ([]domain.ResourceMetricSummary, error) {
	rows, err := s.db.QueryContext(ctx, resourceMetricsQuery)
	if err != nil {
		return nil, fmt.Errorf("list resource metric summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanResourceMetricSummaries(rows)
}

func (s *PostgresStore) ListResourceMetricSummaries(ctx context.Context) ([]domain.ResourceMetricSummary, error) {
	rows, err := s.pool.Query(ctx, resourceMetricsQuery)
	if err != nil {
		return nil, fmt.Errorf("list resource metric summaries: %w", err)
	}
	defer rows.Close()
	return scanResourceMetricSummaries(rows)
}

func scanResourceMetricSummaries(rows rowScanner) ([]domain.ResourceMetricSummary, error) {
	var summaries []domain.ResourceMetricSummary
	for rows.Next() {
		var summary domain.ResourceMetricSummary
		var changed string
		if err := rows.Scan(&summary.Kind, &summary.Source, &summary.State, &summary.Count, &changed); err != nil {
			return nil, fmt.Errorf("scan resource metric summary: %w", err)
		}
		if changed != "" {
			var err error
			summary.OldestPendingAt, err = time.Parse("2006-01-02T15:04:05", changed)
			if err != nil {
				return nil, fmt.Errorf("parse pending resource timestamp: %w", err)
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}
