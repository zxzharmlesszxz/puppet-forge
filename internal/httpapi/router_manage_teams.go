package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

const tokenHistoryPageSize = 20
const maxTokenHistoryPage = 100000
const manageTeamModuleListTarget = "module-list"

type manageTeamSection string

const (
	manageTeamAccessSection  manageTeamSection = "access"
	manageTeamTokensSection  manageTeamSection = "tokens"
	manageTeamModulesSection manageTeamSection = "modules"
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
	query, page, pageSize, err := requestedManageList(req, "teams-list")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	configs, moduleCounts, err := r.loadTeamConfigsAndModuleCounts(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	rows := filterManageTeamSummaries(manageTeamSummaries(configs, moduleCounts, principal), query)
	canonicalPage, changed := normalizedListPage(page, pageSize, len(rows))
	canonicalURL := manageListPageURL("/manage/teams", req.URL.Query(), "teams-list", canonicalPage)
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return
	}
	page = canonicalPage
	pagination, err := manageListPagination("/manage/teams", req.URL.Query(), "teams-list", page, pageSize, len(rows))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows = pageItems(rows, page, pageSize)

	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageTeamsTemplate, manageTeamsData{
		Navigation: r.manageNavigation(req, principal, csrfToken, "teams", ""),
		Principal:  principal,
		Teams:      rows,
		Query:      query,
		Filter: newListFilter(
			"teams-filter", "teams-query", "/manage/teams", "teams-list",
			"Filter teams", "q", "Filter by team or publish space", query, "/manage/teams",
		),
		Pagination: pagination,
		Message:    req.URL.Query().Get("message"),
		Error:      req.URL.Query().Get("error"),
		CSRFToken:  csrfToken,
	})
}

func manageTeamSummaries(configs []auth.TeamConfig, moduleCounts map[string]int, principal auth.Principal) []manageTeamSummary {
	rows := make([]manageTeamSummary, 0, len(configs))
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) || (!principal.CanAdmin && !canManageAccessTeam(principal, cfg.Team)) {
			continue
		}
		spaces := teamPublishSpaces(cfg)
		spaceSet := stringSet(spaces)
		moduleCount := 0
		for space := range spaceSet {
			moduleCount += moduleCounts[space]
		}
		rows = append(rows, manageTeamSummary{
			Team:            cfg.Team,
			Spaces:          spaces,
			ModuleCount:     moduleCount,
			ReadTokens:      len(cfg.ReadTokens) + activeTokenCount(cfg.ReadTokenRecords, time.Now()),
			PublishTokens:   len(cfg.PublishTokens) + activeTokenCount(cfg.PublishTokenRecords, time.Now()),
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
	team, section, canonical, ok := parseManageTeamPath(req.URL.Path)
	if !ok {
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
	teamBasePath := "/manage/teams/" + url.PathEscape(team)
	if !canonical {
		target := teamBasePath + "/" + string(manageTeamAccessSection)
		if req.URL.RawQuery != "" {
			target += "?" + req.URL.RawQuery
		}
		// #nosec G710 -- team is validated by the route parser and escaped as one path segment above.
		http.Redirect(w, req, target, http.StatusFound)
		return
	}
	formRows := accessTeamFormRows([]auth.TeamConfig{*cfg}, principal)
	if len(formRows) != 1 {
		writeError(w, http.StatusForbidden, errors.New("team admin access required"))
		return
	}
	spaces := teamPublishSpaces(*cfg)
	data := manageTeamData{
		Navigation:  r.manageNavigation(req, principal, "", "team-"+string(section), team),
		Principal:   principal,
		Team:        formRows[0],
		Spaces:      spaces,
		Message:     req.URL.Query().Get("message"),
		Error:       req.URL.Query().Get("error"),
		ShowAccess:  section == manageTeamAccessSection,
		ShowTokens:  section == manageTeamTokensSection,
		ShowModules: section == manageTeamModulesSection,
	}

	switch section {
	case manageTeamTokensSection:
		if redirected, err := populateManageTeamTokens(w, req, teamBasePath+"/tokens", &data); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		} else if redirected {
			return
		}
	case manageTeamModulesSection:
		page, pageSize, err := requestedManageModulePage(req, teamBasePath+"/modules", manageTeamModuleListTarget)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if redirected, err := r.populateManageTeamModules(w, req, principal, teamBasePath+"/modules", page, pageSize, &data); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		} else if redirected {
			return
		}
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data.CSRFToken = csrfToken
	if data.ShowModules {
		configureManageModuleRows(data.Modules, csrfToken, teamBasePath+"/modules")
	}
	data.Navigation = r.manageNavigation(req, principal, csrfToken, "team-"+string(section), team)
	executeHTMLTemplate(w, manageTeamTemplate, data)
}

func parseManageTeamPath(path string) (string, manageTeamSection, bool, bool) {
	path = strings.Trim(strings.TrimPrefix(path, "/manage/teams/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], manageTeamAccessSection, false, true
	}
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false, false
	}
	section := manageTeamSection(parts[1])
	switch section {
	case manageTeamAccessSection, manageTeamTokensSection, manageTeamModulesSection:
		return parts[0], section, true, true
	default:
		return "", "", false, false
	}
}

func populateManageTeamTokens(w http.ResponseWriter, req *http.Request, basePath string, data *manageTeamData) (bool, error) {
	tokenPage, err := requestedTokenPage(req, "token_page")
	if err != nil {
		return false, err
	}
	tokenHistoryPage, err := requestedTokenPage(req, "history_page")
	if err != nil {
		return false, err
	}
	tokenPageSize := requestedPageSize(req, basePath, "token_per_page", "active-tokens", tokenHistoryPageSize)
	tokenHistoryPageSize := requestedPageSize(req, basePath, "history_per_page", "token-history", tokenHistoryPageSize)

	data.ActiveTokenQuery = strings.TrimSpace(req.URL.Query().Get("active_token_query"))
	activeTokens := filterTokens(data.Team.Tokens, data.ActiveTokenQuery)
	tokenTotal := len(activeTokens)
	canonicalTokenPage, tokenPageChanged := normalizedListPage(tokenPage, tokenPageSize, tokenTotal)
	canonicalHistoryPage, historyPageChanged := normalizedListPage(tokenHistoryPage, tokenHistoryPageSize, len(filterTokens(data.Team.TokenHistory, strings.TrimSpace(req.URL.Query().Get("token_query")))))
	if tokenPageChanged || historyPageChanged {
		anchor := "token-history"
		if tokenPageChanged {
			anchor = "active-tokens"
		}
		canonicalURL := tokenPagesURL(basePath, req.URL.Query(), canonicalTokenPage, canonicalHistoryPage, anchor)
		if handleCanonicalListPage(w, req, true, canonicalURL) {
			return true, nil
		}
		tokenPage = canonicalTokenPage
		tokenHistoryPage = canonicalHistoryPage
	}
	data.Team.Tokens = pageItems(activeTokens, tokenPage, tokenPageSize)
	tokenHistory := data.Team.TokenHistory
	data.TokenHistoryQuery = strings.TrimSpace(req.URL.Query().Get("token_query"))
	data.TokenHistoryAvailable = len(tokenHistory) > 0 || data.TokenHistoryQuery != ""
	tokenHistory = filterTokens(tokenHistory, data.TokenHistoryQuery)
	tokenHistoryTotal := len(tokenHistory)
	data.TokenHistory = pageItems(tokenHistory, tokenHistoryPage, tokenHistoryPageSize)
	data.TokenPagination = tokenPagination(basePath, req.URL.Query(), "token_page", "token_per_page", "active-tokens", tokenPage, tokenPageSize, tokenTotal)
	data.TokenHistoryPagination = tokenPagination(basePath, req.URL.Query(), "history_page", "history_per_page", "token-history", tokenHistoryPage, tokenHistoryPageSize, tokenHistoryTotal)
	activeTokenParams := tokenFilterParams(req.URL.Query(), "q", "page", "history_page", "token_query")
	activeTokenClearURL := tokenFilterURL(basePath, activeTokenParams, "active-tokens")
	tokenHistoryParams := tokenFilterParams(req.URL.Query(), "q", "page", "token_page", "active_token_query")
	tokenHistoryClearURL := tokenFilterURL(basePath, tokenHistoryParams, "token-history")
	data.ActiveTokenFilter = newListFilter(
		"active-token-filter", "active-token-query", basePath+"#active-tokens", "active-tokens",
		"Filter active tokens", "active_token_query", "Filter by role, status, prefix, or name",
		data.ActiveTokenQuery, activeTokenClearURL, activeTokenParams...,
	)
	data.TokenHistoryFilter = newListFilter(
		"token-history-filter", "token-history-query", basePath+"#token-history", "token-history",
		"Filter revoked and expired tokens", "token_query", "Filter by role, status, prefix, or name",
		data.TokenHistoryQuery, tokenHistoryClearURL, tokenHistoryParams...,
	)
	return false, nil
}

func (r *Router) populateManageTeamModules(w http.ResponseWriter, req *http.Request, principal auth.Principal, basePath string, page, pageSize int, data *manageTeamData) (bool, error) {
	for _, space := range data.Spaces {
		if principal.CanPublishOwner(space) {
			data.PublishSpaces = append(data.PublishSpaces, space)
		}
	}
	data.Query = strings.TrimSpace(req.URL.Query().Get("q"))
	modules, total, err := r.loadManageModuleRows(req.Context(), principal, stringSet(data.Spaces), data.Query, page, pageSize)
	if err != nil {
		return false, err
	}
	canonicalPage, changed := normalizedListPage(page, pageSize, total)
	canonicalURL := managePageURLWithValues(basePath, req.URL.Query(), canonicalPage, manageTeamModuleListTarget)
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return true, nil
	}
	if changed {
		page = canonicalPage
		modules, total, err = r.loadManageModuleRows(req.Context(), principal, stringSet(data.Spaces), data.Query, page, pageSize)
		if err != nil {
			return false, err
		}
	}
	data.Modules = modules
	data.Pagination = managePaginationForRequest(basePath, req.URL.Query(), manageTeamModuleListTarget, manageTeamModuleListTarget, page, pageSize, total)
	data.ModuleFilter = newListFilter(
		"team-module-filter", "team-catalog-query", basePath, manageTeamModuleListTarget,
		"Filter modules", "q", "Filter by owner/name", data.Query, basePath,
	)
	return false, nil
}

func filterTokens(tokens []accessTokenFormRow, query string) []accessTokenFormRow {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return tokens
	}

	filtered := make([]accessTokenFormRow, 0, len(tokens))
	for _, token := range tokens {
		searchable := strings.ToLower(strings.Join([]string{
			token.Kind,
			token.Status,
			token.Prefix,
			token.Description,
		}, "\n"))
		if strings.Contains(searchable, query) {
			filtered = append(filtered, token)
		}
	}
	return filtered
}

func tokenFilterParams(query url.Values, names ...string) []queryParameter {
	params := make([]queryParameter, 0, len(names))
	for _, name := range names {
		for _, value := range query[name] {
			params = append(params, queryParameter{Name: name, Value: value})
		}
	}
	return params
}

func tokenFilterURL(basePath string, params []queryParameter, anchor string) string {
	values := url.Values{}
	for _, param := range params {
		values.Add(param.Name, param.Value)
	}
	if encoded := values.Encode(); encoded != "" {
		return basePath + "?" + encoded + "#" + anchor
	}
	return basePath + "#" + anchor
}

func requestedTokenPage(req *http.Request, parameter string) (int, error) {
	raw := strings.TrimSpace(req.URL.Query().Get(parameter))
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 1 || page > maxTokenHistoryPage {
		return 0, errors.New("invalid token history page")
	}
	return page, nil
}

func tokenPagination(basePath string, query url.Values, pageParameter, sizeParameter, anchor string, page, pageSize, total int) paginationData {
	pageURL := func(target int) string {
		values := url.Values{}
		for key, entries := range query {
			if key == pageParameter || isPageSizeParameter(key) {
				continue
			}
			for _, entry := range entries {
				values.Add(key, entry)
			}
		}
		if target > 1 {
			values.Set(pageParameter, strconv.Itoa(target))
		}
		if encoded := values.Encode(); encoded != "" {
			return basePath + "?" + encoded + "#" + anchor
		}
		return basePath + "#" + anchor
	}
	pagination := paginationFor(page, pageSize, total, pageURL)
	return configurePageSize(pagination, query, basePath, sizeParameter, pageParameter, anchor, anchor)
}

func tokenPagesURL(basePath string, query url.Values, tokenPage, historyPage int, anchor string) string {
	values := url.Values{}
	for key, entries := range query {
		if key == "token_page" || key == "history_page" || isPageSizeParameter(key) || key == "message" || key == "error" {
			continue
		}
		for _, entry := range entries {
			values.Add(key, entry)
		}
	}
	if tokenPage > 1 {
		values.Set("token_page", strconv.Itoa(tokenPage))
	}
	if historyPage > 1 {
		values.Set("history_page", strconv.Itoa(historyPage))
	}
	result := basePath
	if encoded := values.Encode(); encoded != "" {
		result += "?" + encoded
	}
	return result + "#" + anchor
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
