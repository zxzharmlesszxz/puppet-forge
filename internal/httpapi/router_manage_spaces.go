package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"sort"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func (r *Router) managePublishSpacesPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("one team owns one namespace; create a team instead"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin {
		writeError(w, http.StatusForbidden, errors.New("global admin access required"))
		return
	}
	r.renderManagePublishSpaces(w, req, principal, "")
}

func (r *Router) renderManagePublishSpaces(w http.ResponseWriter, req *http.Request, principal auth.Principal, errorMessage string) {
	query, page, pageSize, err := requestedManageList(req, "publish-space-list")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	configs, moduleCounts, err := r.loadTeamConfigsAndModuleCounts(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	upstreamModuleCounts, err := r.modules.CountUpstreamModulesByOwner(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	assignments, teams := managePublishSpaceAssignments(configs, moduleCounts, upstreamModuleCounts)
	assignments = filterManagePublishSpaceAssignments(assignments, query)
	canonicalPage, changed := normalizedListPage(page, pageSize, len(assignments))
	canonicalURL := manageListPageURL("/manage/admin/spaces", req.URL.Query(), "publish-space-list", canonicalPage)
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return
	}
	page = canonicalPage
	pagination, err := manageListPagination("/manage/admin/spaces", req.URL.Query(), "publish-space-list", page, pageSize, len(assignments))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	assignments = pageItems(assignments, page, pageSize)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, managePublishSpacesTemplate, managePublishSpacesData{
		Navigation:  r.manageNavigation(req, principal, csrfToken, "publish-spaces", ""),
		Assignments: assignments,
		Teams:       teams,
		Query:       query,
		Filter: newListFilter(
			"publish-space-filter", "publish-space-query", "/manage/admin/spaces", "publish-space-list",
			"Filter publish spaces", "q", "Filter by team, space, or type", query, "/manage/admin/spaces",
		),
		Pagination: pagination,
		Message:    req.URL.Query().Get("message"),
		Error:      firstNonEmpty(errorMessage, req.URL.Query().Get("error")),
		CSRFToken:  csrfToken,
	})
}

func managePublishSpaceAssignments(configs []auth.TeamConfig, moduleCounts, upstreamModuleCounts map[string]int) ([]managePublishSpaceAssignment, []string) {
	assignments := make([]managePublishSpaceAssignment, 0, len(configs)+len(upstreamModuleCounts))
	teams := make([]string, 0, len(configs))
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) {
			continue
		}
		teams = append(teams, cfg.Team)
		teamURL := "/manage/teams/" + url.PathEscape(cfg.Team) + "/access"
		for _, space := range teamPublishSpaces(cfg) {
			assignments = append(assignments, managePublishSpaceAssignment{
				Team:        cfg.Team,
				TeamURL:     teamURL,
				Space:       space,
				ModulesURL:  "/manage/modules?q=" + url.QueryEscape(space+"/"),
				ModuleCount: moduleCounts[space],
				Primary:     space == cfg.Team,
			})
		}
	}
	for owner, count := range upstreamModuleCounts {
		assignments = append(assignments, managePublishSpaceAssignment{
			Space:       owner,
			ModulesURL:  "/manage/modules?q=" + url.QueryEscape(owner+"/"),
			ModuleCount: count,
			Upstream:    true,
		})
	}
	sort.Strings(teams)
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].Upstream != assignments[j].Upstream {
			return !assignments[i].Upstream
		}
		if assignments[i].Team != assignments[j].Team {
			return assignments[i].Team < assignments[j].Team
		}
		if assignments[i].Primary != assignments[j].Primary {
			return assignments[i].Primary
		}
		return assignments[i].Space < assignments[j].Space
	})
	return assignments, teams
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
