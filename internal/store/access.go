package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanTeamRows(rows rowScanner) ([]auth.TeamConfig, map[string]int, error) {
	var configs []auth.TeamConfig
	index := map[string]int{}
	for rows.Next() {
		var team string
		if err := rows.Scan(&team); err != nil {
			return nil, nil, fmt.Errorf("scan access team: %w", err)
		}
		index[team] = len(configs)
		configs = append(configs, auth.TeamConfig{Team: team})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return configs, index, nil
}

type teamConfigLoader interface {
	loadAccessTokens(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error
	loadAccessOwners(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error
	loadAccessOIDC(ctx context.Context, configs []auth.TeamConfig, index map[string]int) error
}

func loadTeamConfigs(ctx context.Context, rows rowScanner, loader teamConfigLoader) ([]auth.TeamConfig, error) {
	configs, index, err := scanTeamRows(rows)
	if err != nil {
		return nil, err
	}
	if err := loader.loadAccessTokens(ctx, configs, index); err != nil {
		return nil, err
	}
	if err := loader.loadAccessOwners(ctx, configs, index); err != nil {
		return nil, err
	}
	if err := loader.loadAccessOIDC(ctx, configs, index); err != nil {
		return nil, err
	}
	return configs, nil
}

type accessOIDCMapping struct {
	kind  string
	value string
}

func accessOIDCMappings(cfg auth.TeamConfig) []accessOIDCMapping {
	var mappings []accessOIDCMapping
	appendValues := func(kind string, values []string) {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			mappings = append(mappings, accessOIDCMapping{kind: kind, value: value})
		}
	}

	appendValues("email", cfg.OIDCEmails)
	appendValues("subject", cfg.OIDCSubjects)
	appendValues("domain", cfg.OIDCDomains)
	appendValues("group", cfg.OIDCGroups)
	appendValues("team_admin_email", cfg.OIDCTeamAdminEmails)
	appendValues("team_admin_group", cfg.OIDCTeamAdminGroups)
	appendValues("admin_email", cfg.OIDCAdminEmails)
	appendValues("admin_subject", cfg.OIDCAdminSubjects)
	appendValues("admin_group", cfg.OIDCAdminGroups)
	return mappings
}

func accessTokenRecordFromRaw(hasher *auth.TokenHasher, tokenType, token string) (auth.AccessTokenRecord, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return auth.AccessTokenRecord{}, nil
	}
	if hasher == nil {
		return auth.AccessTokenRecord{}, errors.New("access token pepper is required to store access tokens")
	}
	digest := hasher.Digest(token)
	return auth.AccessTokenRecord{
		ID:        uuid.NewString(),
		Prefix:    fmt.Sprintf("legacy_%s_%s", tokenType, digest[:8]),
		Digest:    digest,
		CreatedAt: time.Now().UTC(),
	}, nil
}

type typedAccessTokenRecord struct {
	tokenType string
	record    auth.AccessTokenRecord
}

type accessConfigWriter interface {
	insertTeam(team string) error
	insertToken(team, tokenType string, token auth.AccessTokenRecord) error
	insertOwner(team, owner string) error
	insertOIDCMapping(team string, mapping accessOIDCMapping) error
}

func persistTeamConfigs(configs []auth.TeamConfig, hasher *auth.TokenHasher, writer accessConfigWriter) error {
	for _, cfg := range configs {
		if strings.TrimSpace(cfg.Team) == "" {
			return errors.New("team is required")
		}
		if err := writer.insertTeam(cfg.Team); err != nil {
			return err
		}
		tokens, err := accessTokenRecords(hasher, cfg)
		if err != nil {
			return err
		}
		for _, token := range tokens {
			if err := writer.insertToken(cfg.Team, token.tokenType, token.record); err != nil {
				return err
			}
		}
		for _, owner := range cfg.PublishOwners {
			owner = strings.TrimSpace(owner)
			if owner == "" {
				continue
			}
			if err := writer.insertOwner(cfg.Team, owner); err != nil {
				return err
			}
		}
		for _, mapping := range accessOIDCMappings(cfg) {
			if err := writer.insertOIDCMapping(cfg.Team, mapping); err != nil {
				return err
			}
		}
	}
	return nil
}

func accessTokenRecords(hasher *auth.TokenHasher, cfg auth.TeamConfig) ([]typedAccessTokenRecord, error) {
	records := make([]typedAccessTokenRecord, 0, len(cfg.ReadTokens)+len(cfg.PublishTokens)+len(cfg.ReadTokenRecords)+len(cfg.PublishTokenRecords))
	appendRaw := func(tokenType string, tokens []string) error {
		for _, token := range tokens {
			if strings.TrimSpace(token) == "" {
				continue
			}
			record, err := accessTokenRecordFromRaw(hasher, tokenType, token)
			if err != nil {
				return err
			}
			records = append(records, typedAccessTokenRecord{tokenType: tokenType, record: record})
		}
		return nil
	}
	if err := appendRaw("read", cfg.ReadTokens); err != nil {
		return nil, err
	}
	if err := appendRaw("publish", cfg.PublishTokens); err != nil {
		return nil, err
	}
	for _, record := range cfg.ReadTokenRecords {
		records = append(records, typedAccessTokenRecord{tokenType: "read", record: record})
	}
	for _, record := range cfg.PublishTokenRecords {
		records = append(records, typedAccessTokenRecord{tokenType: "publish", record: record})
	}
	return records, nil
}

func scanArtifactReleaseRows(rows rowScanner) ([]ArtifactReleaseRecord, error) {
	var releases []ArtifactReleaseRecord
	for rows.Next() {
		var release ArtifactReleaseRecord
		if err := rows.Scan(&release.Owner, &release.Name, &release.Version, &release.StoragePath, &release.SHA256, &release.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan artifact release: %w", err)
		}
		releases = append(releases, release)
	}
	return releases, rows.Err()
}

func applyAccessToken(cfg *auth.TeamConfig, tokenType string, token auth.AccessTokenRecord) {
	switch tokenType {
	case "read":
		cfg.ReadTokenRecords = append(cfg.ReadTokenRecords, token)
	case "publish":
		cfg.PublishTokenRecords = append(cfg.PublishTokenRecords, token)
	}
}

func applyAccessOIDCMapping(cfg *auth.TeamConfig, mappingType, value string) {
	switch mappingType {
	case "email":
		cfg.OIDCEmails = append(cfg.OIDCEmails, value)
	case "subject":
		cfg.OIDCSubjects = append(cfg.OIDCSubjects, value)
	case "domain":
		cfg.OIDCDomains = append(cfg.OIDCDomains, value)
	case "group":
		cfg.OIDCGroups = append(cfg.OIDCGroups, value)
	case "team_admin_email":
		cfg.OIDCTeamAdminEmails = append(cfg.OIDCTeamAdminEmails, value)
	case "team_admin_group":
		cfg.OIDCTeamAdminGroups = append(cfg.OIDCTeamAdminGroups, value)
	case "admin_email":
		cfg.OIDCAdminEmails = append(cfg.OIDCAdminEmails, value)
	case "admin_subject":
		cfg.OIDCAdminSubjects = append(cfg.OIDCAdminSubjects, value)
	case "admin_group":
		cfg.OIDCAdminGroups = append(cfg.OIDCAdminGroups, value)
	}
}
