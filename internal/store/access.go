package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"puppet-forge/internal/auth"
)

type teamRowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanTeamRows(rows teamRowScanner) ([]auth.TeamConfig, map[string]int, error) {
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

func loadTeamConfigs(ctx context.Context, rows teamRowScanner, loader teamConfigLoader) ([]auth.TeamConfig, error) {
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

func insertSQLiteAccessToken(ctx context.Context, tx *sql.Tx, team, tokenType, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `insert into access_tokens (team, token_type, token) values (?, ?, ?)`, team, tokenType, token); err != nil {
		return fmt.Errorf("insert access token: %w", err)
	}
	return nil
}

func applyAccessToken(cfg *auth.TeamConfig, tokenType, token string) {
	switch tokenType {
	case "read":
		cfg.ReadTokens = append(cfg.ReadTokens, token)
	case "publish":
		cfg.PublishTokens = append(cfg.PublishTokens, token)
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
