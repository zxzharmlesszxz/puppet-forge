package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func (r *Router) manageAccessPage(w http.ResponseWriter, req *http.Request) {
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin && !principal.CanManageTeam {
		writeError(w, http.StatusForbidden, errors.New("admin access required"))
		return
	}

	switch req.Method {
	case http.MethodGet:
		r.renderManageAccess(w, req, principal, "")
	case http.MethodPost:
		if !requireManageCSRF(w, req) {
			return
		}
		configs, message, err := r.accessConfigsFromForm(req, principal)
		if err != nil {
			r.renderManageAccess(w, req, principal, err.Error())
			return
		}
		if err := r.saveAccessConfigs(req.Context(), configs); err != nil {
			r.renderManageAccess(w, req, principal, err.Error())
			return
		}
		http.Redirect(w, req, "/manage/access?message="+url.QueryEscape(message), http.StatusFound)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (r *Router) manageAccessAddPage(w http.ResponseWriter, req *http.Request) {
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin {
		writeError(w, http.StatusForbidden, errors.New("admin access required"))
		return
	}
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = manageAccessAddTeamTemplate.Execute(w, manageAccessAddTeamData{
		CSRFToken: csrfToken,
		Error:     req.URL.Query().Get("error"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Router) accessConfigsFromForm(req *http.Request, principal auth.Principal) ([]auth.TeamConfig, string, error) {
	switch req.FormValue("action") {
	case "save_global_admins":
		if !principal.CanAdmin {
			return nil, "", errors.New("global admin access required")
		}
		return r.accessConfigsWithGlobalAdmins(req)
	case "save_team":
		return r.accessConfigsWithUpsertedTeam(req, principal)
	case "delete_team":
		if !principal.CanAdmin {
			return nil, "", errors.New("global admin access required")
		}
		return r.accessConfigsWithoutTeam(req)
	case "replace_json", "":
		if !principal.CanAdmin {
			return nil, "", errors.New("global admin access required")
		}
		var configs []auth.TeamConfig
		raw := strings.TrimSpace(req.FormValue("config"))
		if err := json.Unmarshal([]byte(raw), &configs); err != nil {
			return nil, "", fmt.Errorf("parse access config: %w", err)
		}
		return configs, "access config saved", nil
	default:
		return nil, "", errors.New("unknown access form action")
	}
}

func (r *Router) accessConfigsWithUpsertedTeam(req *http.Request, principal auth.Principal) ([]auth.TeamConfig, string, error) {
	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		return nil, "", err
	}
	if err := req.ParseForm(); err != nil {
		return nil, "", fmt.Errorf("parse access form: %w", err)
	}

	originalTeam := strings.TrimSpace(req.FormValue("original_team"))
	cfg := accessTeamConfigFromForm(req)
	if cfg.Team == "" {
		return nil, "", errors.New("team is required")
	}

	if !principal.CanAdmin {
		if !principal.CanManageTeam {
			return nil, "", errors.New("team admin access required")
		}
		if !canManageAccessTeam(principal, originalTeam) || !canManageAccessTeam(principal, cfg.Team) {
			return nil, "", errors.New("team admins can edit only their own team")
		}
		existing := findAccessConfig(configs, originalTeam)
		if existing == nil {
			return nil, "", fmt.Errorf("team %q was not found", originalTeam)
		}
		cfg.PublishOwners = append([]string(nil), existing.PublishOwners...)
		cfg.OIDCEmails = append([]string(nil), existing.OIDCEmails...)
		cfg.OIDCSubjects = append([]string(nil), existing.OIDCSubjects...)
		cfg.OIDCDomains = append([]string(nil), existing.OIDCDomains...)
		cfg.OIDCAdminEmails = append([]string(nil), existing.OIDCAdminEmails...)
		cfg.OIDCAdminSubjects = append([]string(nil), existing.OIDCAdminSubjects...)
		cfg.OIDCAdminGroups = append([]string(nil), existing.OIDCAdminGroups...)
	}

	next := make([]auth.TeamConfig, 0, len(configs)+1)
	for _, existing := range configs {
		if existing.Team == originalTeam || existing.Team == cfg.Team {
			continue
		}
		next = append(next, existing)
	}
	next = append(next, cfg)
	return next, "team access saved", nil
}

func findAccessConfig(configs []auth.TeamConfig, team string) *auth.TeamConfig {
	for i := range configs {
		if configs[i].Team == team {
			return &configs[i]
		}
	}
	return nil
}

func (r *Router) accessConfigsWithoutTeam(req *http.Request) ([]auth.TeamConfig, string, error) {
	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		return nil, "", err
	}

	team := strings.TrimSpace(req.FormValue("team"))
	if team == "" {
		return nil, "", errors.New("team is required")
	}

	next := make([]auth.TeamConfig, 0, len(configs))
	for _, cfg := range configs {
		if cfg.Team != team {
			next = append(next, cfg)
		}
	}
	if len(next) == len(configs) {
		return nil, "", fmt.Errorf("team %q was not found", team)
	}
	return next, "team access deleted", nil
}

func (r *Router) saveAccessConfigs(ctx context.Context, configs []auth.TeamConfig) error {
	if len(configs) == 0 {
		return errors.New("access config must contain at least one team")
	}
	newAuthorizer, err := auth.NewAuthorizer(auth.AccessConfigsWithRuntimeAdmin(configs, r.adminToken))
	if err != nil {
		return err
	}
	if err := r.modules.ReplaceTeamConfigs(ctx, configs); err != nil {
		return err
	}
	r.setAuthorizer(newAuthorizer)
	return nil
}

func (r *Router) renderManageAccess(w http.ResponseWriter, req *http.Request, principal auth.Principal, errorMessage string) {
	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil && errorMessage == "" {
		errorMessage = err.Error()
	}
	raw, marshalErr := json.MarshalIndent(configs, "", "  ")
	if marshalErr != nil && errorMessage == "" {
		errorMessage = marshalErr.Error()
	}
	adminGroups, adminEmails, adminSubjects := globalAdminFormValues(configs)
	csrfToken, csrfErr := r.ensureManageCSRFToken(w, req)
	if csrfErr != nil {
		writeError(w, http.StatusInternalServerError, csrfErr)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = manageAccessTemplate.Execute(w, manageAccessData{
		ConfigJSON:        string(raw),
		Teams:             accessTeamFormRows(configs, principal),
		AdminOIDCGroups:   adminGroups,
		AdminOIDCEmails:   adminEmails,
		AdminOIDCSubjects: adminSubjects,
		CanAdmin:          principal.CanAdmin,
		Message:           req.URL.Query().Get("message"),
		Error:             errorMessage,
		CSRFToken:         csrfToken,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func accessTeamConfigFromForm(req *http.Request) auth.TeamConfig {
	team := strings.TrimSpace(req.FormValue("team"))
	return auth.TeamConfig{
		Team:                team,
		ReadTokens:          splitLineValues(req.FormValue("read_tokens")),
		PublishTokens:       splitLineValues(req.FormValue("publish_tokens")),
		PublishOwners:       publishOwnersFromExtra(team, extraPublishSpacesFromForm(req)),
		OIDCGroups:          splitLineValues(req.FormValue("oidc_groups")),
		OIDCTeamAdminEmails: splitLineValues(req.FormValue("oidc_team_admin_emails")),
		OIDCTeamAdminGroups: splitLineValues(req.FormValue("oidc_team_admin_groups")),
	}
}

func accessTeamFormRows(configs []auth.TeamConfig, principal auth.Principal) []accessTeamFormRow {
	rows := make([]accessTeamFormRow, 0, len(configs))
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) {
			continue
		}
		if !principal.CanAdmin && !canManageAccessTeam(principal, cfg.Team) {
			continue
		}
		extraSpaces := extraPublishOwnersForForm(cfg.Team, cfg.PublishOwners)
		var badges []string
		if n := len(cfg.ReadTokens); n > 0 {
			badges = append(badges, fmt.Sprintf("Read: %d", n))
		}
		if n := len(cfg.PublishTokens); n > 0 {
			badges = append(badges, fmt.Sprintf("Publish: %d", n))
		}
		if n := len(extraSpaces); n > 0 {
			badges = append(badges, fmt.Sprintf("Spaces: %d", n))
		}
		if n := len(cfg.OIDCGroups) + len(cfg.OIDCTeamAdminEmails) + len(cfg.OIDCTeamAdminGroups); n > 0 {
			badges = append(badges, fmt.Sprintf("OIDC: %d", n))
		}
		rows = append(rows, accessTeamFormRow{
			Team:                cfg.Team,
			ReadTokens:          joinLineValues(cfg.ReadTokens),
			PublishTokens:       joinLineValues(cfg.PublishTokens),
			ExtraPublishSpaces:  joinLineValues(extraSpaces),
			OIDCGroups:          joinLineValues(cfg.OIDCGroups),
			OIDCTeamAdminEmails: joinLineValues(cfg.OIDCTeamAdminEmails),
			OIDCTeamAdminGroups: joinLineValues(cfg.OIDCTeamAdminGroups),
			Badges:              badges,
		})
	}
	return rows
}

func extraPublishSpacesFromForm(req *http.Request) []string {
	return splitLineValues(req.FormValue("extra_publish_spaces"))
}

func publishOwnersFromExtra(team string, extraOwners []string) []string {
	team = strings.TrimSpace(team)
	if team == "" && len(extraOwners) == 0 {
		return nil
	}

	owners := make([]string, 0, len(extraOwners)+1)
	seen := make(map[string]struct{}, len(extraOwners)+1)
	add := func(owner string) {
		owner = strings.TrimSpace(owner)
		if owner == "" {
			return
		}
		if _, exists := seen[owner]; exists {
			return
		}
		seen[owner] = struct{}{}
		owners = append(owners, owner)
	}

	add(team)
	for _, owner := range extraOwners {
		add(owner)
	}
	return owners
}

func extraPublishOwnersForForm(team string, owners []string) []string {
	team = strings.TrimSpace(team)
	extra := make([]string, 0, len(owners))
	for _, owner := range owners {
		owner = strings.TrimSpace(owner)
		if owner == "" || owner == team {
			continue
		}
		extra = append(extra, owner)
	}
	return extra
}

func (r *Router) accessConfigsWithGlobalAdmins(req *http.Request) ([]auth.TeamConfig, string, error) {
	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		return nil, "", err
	}
	if err := req.ParseForm(); err != nil {
		return nil, "", fmt.Errorf("parse admin access form: %w", err)
	}

	adminCfg := auth.TeamConfig{
		Team:              "platform-admin",
		OIDCAdminGroups:   splitLineValues(req.FormValue("oidc_admin_groups")),
		OIDCAdminEmails:   splitLineValues(req.FormValue("oidc_admin_emails")),
		OIDCAdminSubjects: splitLineValues(req.FormValue("oidc_admin_subjects")),
	}

	next := make([]auth.TeamConfig, 0, len(configs)+1)
	for _, cfg := range configs {
		cfg.OIDCAdminGroups = nil
		cfg.OIDCAdminEmails = nil
		cfg.OIDCAdminSubjects = nil
		if isEmptyAccessConfig(cfg) {
			continue
		}
		next = append(next, cfg)
	}
	if !isEmptyAccessConfig(adminCfg) {
		next = append(next, adminCfg)
	}

	return next, "global admin access saved", nil
}

func globalAdminFormValues(configs []auth.TeamConfig) (groups, emails, subjects string) {
	var adminGroups []string
	var adminEmails []string
	var adminSubjects []string
	for _, cfg := range configs {
		adminGroups = append(adminGroups, cfg.OIDCAdminGroups...)
		adminEmails = append(adminEmails, cfg.OIDCAdminEmails...)
		adminSubjects = append(adminSubjects, cfg.OIDCAdminSubjects...)
	}
	return joinLineValues(adminGroups), joinLineValues(adminEmails), joinLineValues(adminSubjects)
}

func hasNoAccessConfig(cfg auth.TeamConfig) bool {
	return len(cfg.ReadTokens) == 0 &&
		len(cfg.PublishTokens) == 0 &&
		len(cfg.PublishOwners) == 0 &&
		len(cfg.OIDCGroups) == 0 &&
		len(cfg.OIDCTeamAdminEmails) == 0 &&
		len(cfg.OIDCTeamAdminGroups) == 0 &&
		len(cfg.OIDCEmails) == 0 &&
		len(cfg.OIDCSubjects) == 0 &&
		len(cfg.OIDCDomains) == 0
}

func isGlobalAdminConfig(cfg auth.TeamConfig) bool {
	return hasNoAccessConfig(cfg) &&
		(len(cfg.OIDCAdminGroups) > 0 || len(cfg.OIDCAdminEmails) > 0 || len(cfg.OIDCAdminSubjects) > 0)
}

func isEmptyAccessConfig(cfg auth.TeamConfig) bool {
	return hasNoAccessConfig(cfg) &&
		len(cfg.AdminTokens) == 0 &&
		len(cfg.OIDCAdminGroups) == 0 &&
		len(cfg.OIDCAdminEmails) == 0 &&
		len(cfg.OIDCAdminSubjects) == 0
}

func splitLineValues(raw string) []string {
	lines := strings.Split(raw, "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		value := strings.TrimSpace(line)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func joinLineValues(values []string) string {
	return strings.Join(values, "\n")
}

// testableRandomBase64URL allows tests to inject a mock function.
