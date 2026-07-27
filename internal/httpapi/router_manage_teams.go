package httpapi

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func (r *Router) manageTeamsPage(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/manage/teams" {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin && !principal.CanManageTeam {
		writeError(w, http.StatusForbidden, errors.New("team admin access required"))
		return
	}

	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	modules, err := r.modules.ListModules(req.Context(), 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	rows := manageTeamSummaries(configs, modules, principal)

	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := manageTeamsTemplate.Execute(w, manageTeamsData{
		Principal: principal,
		Teams:     rows,
		Message:   req.URL.Query().Get("message"),
		Error:     req.URL.Query().Get("error"),
		CSRFToken: csrfToken,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func manageTeamSummaries(configs []auth.TeamConfig, modules []domain.Module, principal auth.Principal) []manageTeamSummary {
	rows := make([]manageTeamSummary, 0, len(configs))
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) || (!principal.CanAdmin && !canManageAccessTeam(principal, cfg.Team)) {
			continue
		}
		spaces := teamPublishSpaces(cfg)
		spaceSet := stringSet(spaces)
		moduleCount := 0
		for _, module := range modules {
			if _, belongs := spaceSet[module.Owner]; belongs {
				moduleCount++
			}
		}
		rows = append(rows, manageTeamSummary{
			Team:            cfg.Team,
			Spaces:          spaces,
			ModuleCount:     moduleCount,
			ReadTokens:      len(cfg.ReadTokens),
			PublishTokens:   len(cfg.PublishTokens),
			PublishGroups:   len(cfg.OIDCGroups),
			TeamAdminUsers:  len(cfg.OIDCTeamAdminEmails),
			TeamAdminGroups: len(cfg.OIDCTeamAdminGroups),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Team < rows[j].Team })
	return rows
}

func (r *Router) manageTeamPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	team := strings.Trim(strings.TrimPrefix(req.URL.Path, "/manage/teams/"), "/")
	if team == "" || strings.Contains(team, "/") {
		writeError(w, http.StatusNotFound, errors.New("team not found"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin && !canManageAccessTeam(principal, team) {
		writeError(w, http.StatusForbidden, errors.New("team admin access required"))
		return
	}

	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfg := findAccessConfig(configs, team)
	if cfg == nil || isGlobalAdminConfig(*cfg) {
		writeError(w, http.StatusNotFound, errors.New("team not found"))
		return
	}
	formRows := accessTeamFormRows([]auth.TeamConfig{*cfg}, principal)
	if len(formRows) != 1 {
		writeError(w, http.StatusForbidden, errors.New("team admin access required"))
		return
	}
	spaces := teamPublishSpaces(*cfg)
	publishSpaces := make([]string, 0, len(spaces))
	for _, space := range spaces {
		if principal.CanPublishOwner(space) {
			publishSpaces = append(publishSpaces, space)
		}
	}
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	modules, err := r.loadManageModuleRows(req.Context(), principal, stringSet(spaces), query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := manageTeamTemplate.Execute(w, manageTeamData{
		Principal:     principal,
		Team:          formRows[0],
		Spaces:        spaces,
		PublishSpaces: publishSpaces,
		Modules:       modules,
		Message:       req.URL.Query().Get("message"),
		Error:         req.URL.Query().Get("error"),
		CSRFToken:     csrfToken,
		Query:         query,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func teamPublishSpaces(cfg auth.TeamConfig) []string {
	spaces := publishOwnersFromExtra(cfg.Team, cfg.PublishOwners)
	sort.Strings(spaces)
	return spaces
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}
