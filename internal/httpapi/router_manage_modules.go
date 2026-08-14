package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

const (
	manageModulePageSize   = 50
	manageModuleListTarget = "manage-module-list"
)

func (r *Router) manageModulesPage(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/manage/modules" {
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

	owners := manageableOwners(principal)
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	page, pageSize, err := requestedManageModulePage(req, "/manage/modules", manageModuleListTarget)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows, total, err := r.loadManageModuleRows(req.Context(), principal, nil, query, page, pageSize)
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
	executeHTMLTemplate(w, managePageTemplate, managePageData{
		Navigation: r.manageNavigation(req, principal, csrfToken, "modules", ""),
		Principal:  principal,
		Teams:      manageableTeams(principal),
		Owners:     owners,
		Modules:    rows,
		Message:    req.URL.Query().Get("message"),
		Error:      req.URL.Query().Get("error"),
		CSRFToken:  csrfToken,
		Query:      query,
		Pagination: managePaginationForRequest("/manage/modules", req.URL.Query(), manageModuleListTarget, manageModuleListTarget, page, pageSize, total),
	})
}

func (r *Router) manageModulesRoot(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		r.manageModulesPage(w, req)
	case http.MethodPost:
		r.manageModules(w, req)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (r *Router) loadManageModuleRows(ctx context.Context, principal auth.Principal, allowedOwners map[string]struct{}, query string, page, pageSize int) ([]manageModuleRow, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var owners []string
	if allowedOwners != nil {
		owners = make([]string, 0, len(allowedOwners))
		for owner := range allowedOwners {
			owners = append(owners, owner)
		}
		sort.Strings(owners)
	} else if !principal.CanAdmin {
		owners = manageableOwners(principal)
	}

	modules, total, err := r.modules.ListModulesPageFiltered(ctx, owners, query, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	activeReleases, err := r.modules.ListActiveReleasesForModules(ctx, time.Now().Add(-r.activeReleaseTTL), modules)
	if err != nil {
		return nil, 0, err
	}
	activeReleaseSet := make(map[struct{ owner, name, version string }]struct{}, len(activeReleases))
	for _, release := range activeReleases {
		activeReleaseSet[struct{ owner, name, version string }{release.Owner, release.Name, release.Version}] = struct{}{}
	}
	releasesByModule, err := r.modules.ListReleasesForModules(ctx, modules)
	if err != nil {
		return nil, 0, err
	}

	rows := make([]manageModuleRow, 0, len(modules))
	for _, module := range modules {
		versions := releasesByModule[module.Owner+"\x00"+module.Name]
		versionRows := make([]manageVersionRow, 0, len(versions))
		for _, version := range versions {
			_, active := activeReleaseSet[struct{ owner, name, version string }{module.Owner, module.Name, version.Version}]
			versionRows = append(versionRows, manageVersionRow{
				Version: version.Version,
				Active:  active,
				Latest:  version.Version == module.LatestVersion,
			})
		}
		rows = append(rows, manageModuleRow{
			Module:    module,
			Versions:  versionRows,
			CanDelete: canDeleteInSpace(principal, module.Owner),
		})
	}
	return rows, total, nil
}

func requestedManagePageWithSize(req *http.Request, pageSize int) (int, error) {
	raw := strings.TrimSpace(req.URL.Query().Get("page"))
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	maxPage := store.MaxModulePageOffset/pageSize + 1
	if err != nil || page < 1 || page > maxPage {
		return 0, errors.New("invalid page")
	}
	return page, nil
}

func requestedManageModulePage(req *http.Request, action, target string) (int, int, error) {
	pageSize := requestedPageSize(req, action, "per_page", target, manageModulePageSize)
	page, err := requestedManagePageWithSize(req, pageSize)
	return page, pageSize, err
}

func managePaginationForRequest(basePath string, current url.Values, anchor, target string, page, pageSize, total int) paginationData {
	pagination := paginationFor(page, pageSize, total, func(targetPage int) string {
		return managePageURLWithValues(basePath, current, targetPage, anchor)
	})
	return configurePageSize(pagination, current, basePath, "per_page", "page", anchor, target)
}

func managePageURLWithValues(basePath string, current url.Values, page int, anchor string) string {
	values := url.Values{}
	for key, entries := range current {
		if key == "page" || isPageSizeParameter(key) || key == "message" || key == "error" {
			continue
		}
		for _, value := range entries {
			values.Add(key, value)
		}
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	} else {
		values.Del("page")
	}
	result := basePath
	if encoded := values.Encode(); encoded != "" {
		result += "?" + encoded
	}
	if anchor != "" {
		result += "#" + anchor
	}
	return result
}

func (r *Router) manageModules(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/manage/modules" {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !r.requireManageCSRF(w, req) {
		return
	}
	allowed, rateLimitErr := r.allowSharedRateLimit(req, "manage-publish", 60, time.Minute)
	if rateLimitErr != nil {
		redirectManageError(w, req, errors.New("publish rate limit service is unavailable"))
		return
	}
	if !allowed {
		redirectManageError(w, req, errors.New("too many publish attempts"))
		return
	}

	input, cleanup, err := readPublishInput(w, req, r.moduleUploadMax)
	if err != nil {
		r.audit(req, principal, "publish_module", "failure", auditReason(err))
		if isRequestTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		redirectManageError(w, req, err)
		return
	}
	defer cleanup()
	if !principal.CanPublishOwner(input.Owner) {
		r.audit(req, principal, "publish_module", "failure", "forbidden", "space", input.Owner)
		redirectManageError(w, req, errors.New("token is not allowed to publish to this space"))
		return
	}
	release, err := r.modules.Publish(req.Context(), input)
	if err != nil {
		r.audit(req, principal, "publish_module", "failure", auditReason(err), "space", input.Owner)
		redirectManageError(w, req, err)
		return
	}
	r.audit(req, principal, "publish_module", "success", "none", "space", release.Owner, "module", release.Owner+"/"+release.Name, "release", release.Version, "sha256", release.SHA256)
	redirectManageResult(w, req, "/manage/modules", "message", "module published")
}

func (r *Router) manageUpstreamModule(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/manage/upstream" {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !r.requireManageCSRF(w, req) {
		return
	}
	if !principal.CanAdmin {
		redirectManageError(w, req, errors.New("global admin access required"))
		return
	}

	owner, name, err := parseUpstreamModuleFormValue(req.FormValue("module"))
	if err != nil {
		redirectManageError(w, req, err)
		return
	}
	if err := r.modules.SyncUpstreamModule(req.Context(), owner, name); err != nil {
		r.audit(req, principal, "import_upstream_module", "failure", auditReason(err), "module", owner+"/"+name)
		redirectManageError(w, req, err)
		return
	}
	r.audit(req, principal, "import_upstream_module", "success", "none", "module", owner+"/"+name)
	http.Redirect(w, req, "/manage/modules?message="+url.QueryEscape("upstream module added"), http.StatusFound)
}

func (r *Router) manageModuleAction(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !r.requireManageCSRF(w, req) {
		return
	}

	trimmed := strings.Trim(strings.TrimPrefix(req.URL.Path, "/manage/modules/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 3 && parts[2] == "delete" {
		owner, name := parts[0], parts[1]
		if !canDeleteInSpace(principal, owner) {
			redirectManageError(w, req, errors.New("admin or team admin access required"))
			return
		}
		if err := r.modules.DeleteModuleIfAllowed(req.Context(), owner, name, time.Now().Add(-r.activeReleaseTTL)); err != nil {
			r.audit(req, principal, "delete_module", "failure", auditReason(err), "space", owner, "module", owner+"/"+name)
			redirectManageError(w, req, err)
			return
		}
		r.audit(req, principal, "delete_module", "success", "none", "space", owner, "module", owner+"/"+name)
		redirectManageResult(w, req, "/manage/modules", "message", "module deleted")
		return
	}
	if len(parts) == 5 && parts[2] == "versions" && parts[4] == "delete" {
		owner, name, version := parts[0], parts[1], parts[3]
		if !canDeleteInSpace(principal, owner) {
			redirectManageError(w, req, errors.New("admin or team admin access required"))
			return
		}
		if err := r.modules.DeleteReleaseIfAllowed(req.Context(), owner, name, version, time.Now().Add(-r.activeReleaseTTL)); err != nil {
			r.audit(req, principal, "delete_release", "failure", auditReason(err), "space", owner, "module", owner+"/"+name, "release", version)
			redirectManageError(w, req, err)
			return
		}
		r.audit(req, principal, "delete_release", "success", "none", "space", owner, "module", owner+"/"+name, "release", version)
		redirectManageResult(w, req, "/manage/modules", "message", "version deleted")
		return
	}

	writeError(w, http.StatusNotFound, errors.New("route not found"))
}

func parseUpstreamModuleFormValue(raw string) (string, string, error) {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", "", errors.New("upstream module is required")
	}

	var owner, name string
	if strings.Contains(raw, "/") {
		parts := strings.Split(raw, "/")
		if len(parts) != 2 {
			return "", "", errors.New("upstream module must be owner/name or owner-name")
		}
		owner, name = parts[0], parts[1]
	} else {
		owner, name, _ = strings.Cut(raw, "-")
	}

	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if owner == "" || name == "" {
		return "", "", errors.New("upstream module must be owner/name or owner-name")
	}
	if strings.Contains(owner, "/") || strings.Contains(name, "/") {
		return "", "", errors.New("upstream module must be owner/name or owner-name")
	}
	return owner, name, nil
}

func redirectManageError(w http.ResponseWriter, req *http.Request, err error) {
	redirectManageResult(w, req, "/manage/modules", "error", err.Error())
}

func redirectManageResult(w http.ResponseWriter, req *http.Request, fallback, key, message string) {
	target := manageReturnPath(req, fallback)
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	// #nosec G710 -- manageReturnPath accepts only validated local manage paths or a fixed local fallback.
	http.Redirect(w, req, target+separator+url.QueryEscape(key)+"="+url.QueryEscape(message), http.StatusFound)
}

func manageReturnPath(req *http.Request, fallback string) string {
	next := strings.TrimSpace(req.FormValue("next"))
	if next == "/manage/modules" || next == "/manage/teams" || strings.HasPrefix(next, "/manage/teams/") || next == "/manage/admin/access" {
		parsed, err := url.ParseRequestURI(next)
		if err == nil && !parsed.IsAbs() && parsed.Host == "" && !strings.HasPrefix(next, "//") {
			return next
		}
	}
	return fallback
}

func canManageAccessTeam(principal auth.Principal, team string) bool {
	if principal.CanAdmin {
		return true
	}
	if !principal.CanManageTeam {
		return false
	}
	if _, ok := principal.ManagedTeams[team]; ok {
		return true
	}
	return principal.Team == team
}

func canDeleteInSpace(principal auth.Principal, owner string) bool {
	return principal.CanDeleteOwner(owner)
}

func manageableOwners(principal auth.Principal) []string {
	owners := make([]string, 0, len(principal.PublishOwners))
	for owner := range principal.PublishOwners {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}

func manageableTeams(principal auth.Principal) []string {
	teams := make([]string, 0, max(1, len(principal.ManagedTeams)))
	for team := range principal.ManagedTeams {
		teams = append(teams, team)
	}
	if len(teams) == 0 && principal.Team != "" && !principal.CanAdmin {
		teams = append(teams, principal.Team)
	}
	sort.Strings(teams)
	return teams
}
