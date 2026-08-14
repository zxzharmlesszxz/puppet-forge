package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

const (
	manageOverviewModulePageSize = 20
	manageOverviewTeamPageSize   = 10
	manageOverviewSpacePageSize  = 10
)

func (r *Router) managePage(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/manage" {
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
	modulePageSize := requestedPageSize(req, "/manage", "modules_per_page", "", manageOverviewModulePageSize)
	teamPageSize := requestedPageSize(req, "/manage", "teams_per_page", "", manageOverviewTeamPageSize)
	spacePageSize := requestedPageSize(req, "/manage", "spaces_per_page", "", manageOverviewSpacePageSize)
	modulePage, err := requestedOverviewPage(req, "modules_page", modulePageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	teamPage, err := requestedOverviewPage(req, "teams_page", teamPageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	spacePage, err := requestedOverviewPage(req, "spaces_page", spacePageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()

	configs, err := r.modules.LoadTeamConfigs(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	modules, moduleTotal, err := r.manageOverviewModules(ctx, principal, configs, modulePage, modulePageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if redirectOutOfRangeOverviewPage(w, req, modulePage, moduleTotal, modulePageSize, "modules_page", "modules") {
		return
	}
	moduleCounts, err := r.modules.CountModulesByOwner(ctx, manageOverviewTeamOwners(configs, principal))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	spaces := manageOverviewSpaces(configs, moduleCounts, principal)
	spaceTotal := len(spaces)
	if redirectOutOfRangeOverviewPage(w, req, spacePage, spaceTotal, spacePageSize, "spaces_page", "spaces") {
		return
	}
	spaces = pageItems(spaces, spacePage, spacePageSize)

	var teams []manageOverviewTeamSummary
	teamTotal := 0
	if principal.CanAdmin || principal.CanManageTeam {
		teams = manageOverviewTeams(manageTeamSummaries(configs, moduleCounts, principal))
		teamTotal = len(teams)
	}
	if redirectOutOfRangeOverviewPage(w, req, teamPage, teamTotal, teamPageSize, "teams_page", "teams") {
		return
	}
	teams = pageItems(teams, teamPage, teamPageSize)

	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageOverviewTemplate, manageOverviewData{
		Navigation:  r.manageNavigation(req, principal, csrfToken, "overview", ""),
		Modules:     modules,
		ModuleTotal: moduleTotal,
		ModulePages: overviewPagination(req.URL.Query(), "modules_page", "modules_per_page", "modules", modulePage, moduleTotal, modulePageSize),
		Spaces:      spaces,
		SpaceTotal:  spaceTotal,
		SpacePages:  overviewPagination(req.URL.Query(), "spaces_page", "spaces_per_page", "spaces", spacePage, spaceTotal, spacePageSize),
		Teams:       teams,
		TeamTotal:   teamTotal,
		TeamPages:   overviewPagination(req.URL.Query(), "teams_page", "teams_per_page", "teams", teamPage, teamTotal, teamPageSize),
	})
}

func (r *Router) manageOverviewModules(ctx context.Context, principal auth.Principal, configs []auth.TeamConfig, page, pageSize int) ([]domain.Module, int, error) {
	var owners []string
	if !principal.CanAdmin {
		owners = manageableOwners(principal)
	}
	teamOwners := manageOverviewTeamOwners(configs, principal)
	return r.modules.ListModulesPagePrioritized(
		ctx,
		owners,
		teamOwners,
		"",
		pageSize,
		(page-1)*pageSize,
	)
}

func manageOverviewTeamOwners(configs []auth.TeamConfig, principal auth.Principal) []string {
	owners := make(map[string]struct{})
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) || (!principal.CanAdmin && !canManageAccessTeam(principal, cfg.Team)) {
			continue
		}
		for _, owner := range teamPublishSpaces(cfg) {
			owners[owner] = struct{}{}
		}
	}

	result := make([]string, 0, len(owners))
	for owner := range owners {
		result = append(result, owner)
	}
	sort.Strings(result)
	return result
}

func manageOverviewSpaces(configs []auth.TeamConfig, moduleCounts map[string]int, principal auth.Principal) []manageOverviewSpaceSummary {
	visible := make(map[string]struct{}, len(principal.PublishOwners))
	if !principal.CanAdmin {
		for space := range principal.PublishOwners {
			visible[space] = struct{}{}
		}
	}
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) || (!principal.CanAdmin && !canManageAccessTeam(principal, cfg.Team)) {
			continue
		}
		for _, space := range teamPublishSpaces(cfg) {
			visible[space] = struct{}{}
		}
	}

	spaces := make([]manageOverviewSpaceSummary, 0, len(visible))
	for space := range visible {
		spaces = append(spaces, manageOverviewSpaceSummary{
			Name:        space,
			ModuleCount: moduleCounts[space],
			ModulesURL:  "/manage/modules?q=" + url.QueryEscape(space+"/"),
		})
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].Name < spaces[j].Name })
	return spaces
}

func requestedOverviewPage(req *http.Request, parameter string, pageSize int) (int, error) {
	raw := req.URL.Query().Get(parameter)
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	maxPage := store.MaxModulePageOffset/pageSize + 1
	if err != nil || page < 1 || page > maxPage {
		return 0, errors.New("invalid " + parameter)
	}
	return page, nil
}

func redirectOutOfRangeOverviewPage(w http.ResponseWriter, req *http.Request, page, total, pageSize int, parameter, anchor string) bool {
	if validateOverviewPage(page, total, pageSize, parameter) == nil {
		return false
	}
	totalPages := max(1, (total+pageSize-1)/pageSize)
	http.Redirect(w, req, overviewPageURL(req.URL.Query(), parameter, totalPages, anchor), http.StatusSeeOther)
	return true
}

func validateOverviewPage(page, total, pageSize int, parameter string) error {
	totalPages := max(1, (total+pageSize-1)/pageSize)
	if page > totalPages {
		return errors.New("invalid " + parameter)
	}
	return nil
}

func overviewPagination(current url.Values, pageParameter, sizeParameter, anchor string, page, total, pageSize int) paginationData {
	pagination := paginationFor(page, pageSize, total, func(targetPage int) string {
		return overviewPageURL(current, pageParameter, targetPage, anchor)
	})
	return configurePageSize(pagination, current, "/manage", sizeParameter, pageParameter, anchor, "")
}

func overviewPageURL(current url.Values, parameter string, page int, anchor string) string {
	values := make(url.Values, 6)
	for _, name := range []string{"modules_page", "teams_page", "spaces_page"} {
		if value := current.Get(name); value != "" {
			values.Set(name, value)
		}
	}
	if page > 1 {
		values.Set(parameter, strconv.Itoa(page))
	} else {
		values.Del(parameter)
	}
	path := "/manage"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path + "#" + anchor
}

func manageOverviewTeams(summaries []manageTeamSummary) []manageOverviewTeamSummary {
	teams := make([]manageOverviewTeamSummary, 0, len(summaries))
	for _, summary := range summaries {
		teamURL := "/manage/teams/" + url.PathEscape(summary.Team)
		teams = append(teams, manageOverviewTeamSummary{
			Name:        summary.Team,
			ModuleCount: summary.ModuleCount,
			TeamURL:     teamURL + "/access",
			ModulesURL:  teamURL + "/modules",
		})
	}
	return teams
}
