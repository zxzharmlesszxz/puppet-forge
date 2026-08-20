package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func (r *Router) managePublishSpacesPage(w http.ResponseWriter, req *http.Request) {
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin {
		writeError(w, http.StatusForbidden, errors.New("global admin access required"))
		return
	}

	switch req.Method {
	case http.MethodGet:
		r.renderManagePublishSpaces(w, req, principal, "")
	case http.MethodPost:
		if !r.requireManageCSRF(w, req) {
			return
		}
		unlock, err := r.modules.LockAccessConfig(req.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		defer releaseAccessConfigLock(unlock)
		configs, message, err := r.publishSpaceConfigsFromForm(req)
		if err == nil {
			err = r.saveAccessConfigs(req.Context(), configs)
		}
		if err != nil {
			r.audit(req, principal, "change_publish_spaces", "failure", auditReason(err), "team", req.FormValue("team"), "space", req.FormValue("space"))
			redirectManageResult(w, req, "/manage/admin/spaces", "error", err.Error())
			return
		}
		r.audit(req, principal, "change_publish_spaces", "success", "none", "team", req.FormValue("team"), "space", req.FormValue("space"))
		redirectManageResult(w, req, "/manage/admin/spaces", "message", message)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
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

func (r *Router) publishSpaceConfigsFromForm(req *http.Request) ([]auth.TeamConfig, string, error) {
	if err := req.ParseForm(); err != nil {
		return nil, "", fmt.Errorf("parse publish space form: %w", err)
	}
	team := strings.TrimSpace(req.FormValue("team"))
	space := strings.TrimSpace(req.FormValue("space"))
	if team == "" {
		return nil, "", errors.New("team is required")
	}
	if !domain.ValidModuleOwner(space) {
		return nil, "", errors.New("publish space must contain only ASCII letters and digits")
	}
	if len(space) > 128 {
		return nil, "", errors.New("publish space must not exceed 128 characters")
	}

	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		return nil, "", err
	}
	cfg := findAccessConfig(configs, team)
	if cfg == nil || isGlobalAdminConfig(*cfg) {
		return nil, "", fmt.Errorf("team %q was not found", team)
	}
	if space == cfg.Team {
		return nil, "", errors.New("the primary team space is managed automatically")
	}
	if primary := findAccessConfig(configs, space); primary != nil && primary.Team != cfg.Team {
		return nil, "", fmt.Errorf("publish space %q is the primary space of another team", space)
	}

	action := strings.TrimSpace(req.FormValue("action"))
	if action == "assign" {
		upstreamSpaces, err := r.modules.CountUpstreamModulesByOwner(req.Context())
		if err != nil {
			return nil, "", err
		}
		if _, upstream := upstreamSpaces[space]; upstream {
			return nil, "", fmt.Errorf("publish space %q belongs to Official Forge and is read only", space)
		}
	}

	extras := extraPublishOwnersForForm(cfg.Team, cfg.PublishOwners)
	switch action {
	case "assign":
		if !containsString(extras, space) {
			extras = append(extras, space)
		}
		sort.Strings(extras)
		cfg.PublishOwners = publishOwnersFromExtra(cfg.Team, extras)
		return configs, "publish space assigned", nil
	case "unassign":
		if !containsString(extras, space) {
			return nil, "", fmt.Errorf("publish space %q is not assigned to team %q", space, team)
		}
		next := extras[:0]
		for _, existing := range extras {
			if existing != space {
				next = append(next, existing)
			}
		}
		cfg.PublishOwners = publishOwnersFromExtra(cfg.Team, next)
		return configs, "publish space unassigned", nil
	default:
		return nil, "", errors.New("unknown publish space action")
	}
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

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
