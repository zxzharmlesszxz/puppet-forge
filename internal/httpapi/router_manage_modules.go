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
)

const manageModulePageSize = 50

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

	owners := manageableOwners(principal)
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	page, err := requestedManagePage(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows, total, err := r.loadManageModuleRows(req.Context(), principal, nil, query, page)
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
	err = managePageTemplate.Execute(w, managePageData{
		Principal:  principal,
		Owners:     owners,
		Modules:    rows,
		Message:    req.URL.Query().Get("message"),
		Error:      req.URL.Query().Get("error"),
		CSRFToken:  csrfToken,
		Query:      query,
		Pagination: managePagination("/manage", query, page, total),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Router) loadManageModuleRows(ctx context.Context, principal auth.Principal, allowedOwners map[string]struct{}, query string, page int) ([]manageModuleRow, int, error) {
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

	modules, total, err := r.modules.ListModulesPageFiltered(ctx, owners, query, manageModulePageSize, (page-1)*manageModulePageSize)
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

	rows := make([]manageModuleRow, 0, len(modules))
	for _, module := range modules {
		versions, err := r.modules.ListReleases(ctx, module.Owner, module.Name)
		if err != nil {
			return nil, 0, err
		}
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

func requestedManagePage(req *http.Request) (int, error) {
	raw := strings.TrimSpace(req.URL.Query().Get("page"))
	if raw == "" {
		return 1, nil
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 1 {
		return 0, errors.New("invalid page")
	}
	return page, nil
}

func managePagination(basePath, query string, page, total int) paginationData {
	totalPages := (total + manageModulePageSize - 1) / manageModulePageSize
	pagination := paginationData{
		Page:       page,
		Total:      total,
		TotalPages: totalPages,
		HasPrev:    page > 1,
		HasNext:    page < totalPages,
	}
	if pagination.HasPrev {
		pagination.PrevURL = managePageURL(basePath, query, page-1)
	}
	if pagination.HasNext {
		pagination.NextURL = managePageURL(basePath, query, page+1)
	}
	return pagination
}

func managePageURL(basePath, query string, page int) string {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return basePath + "?" + encoded
	}
	return basePath
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
	if !requireManageCSRF(w, req) {
		return
	}
	if !r.rateLimiter.Allow(rateLimitKey(req, "manage-publish"), 60, time.Minute) {
		redirectManageError(w, req, errors.New("too many publish attempts"))
		return
	}

	input, err := readPublishInput(w, req, r.moduleUploadMax)
	if err != nil {
		if isRequestTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		redirectManageError(w, req, err)
		return
	}
	if !principal.CanPublishOwner(input.Owner) {
		redirectManageError(w, req, errors.New("token is not allowed to publish to this space"))
		return
	}
	if _, err := r.modules.Publish(req.Context(), input); err != nil {
		redirectManageError(w, req, err)
		return
	}
	redirectManageResult(w, req, "/manage", "message", "module published")
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
	if !requireManageCSRF(w, req) {
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
		redirectManageError(w, req, err)
		return
	}
	http.Redirect(w, req, "/manage?message="+url.QueryEscape("upstream module added"), http.StatusFound)
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
	if !requireManageCSRF(w, req) {
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
		if err := r.ensureModuleDeletable(req.Context(), owner, name); err != nil {
			redirectManageError(w, req, err)
			return
		}
		if err := r.modules.DeleteModule(req.Context(), owner, name); err != nil {
			redirectManageError(w, req, err)
			return
		}
		redirectManageResult(w, req, "/manage", "message", "module deleted")
		return
	}
	if len(parts) == 5 && parts[2] == "versions" && parts[4] == "delete" {
		owner, name, version := parts[0], parts[1], parts[3]
		if !canDeleteInSpace(principal, owner) {
			redirectManageError(w, req, errors.New("admin or team admin access required"))
			return
		}
		if err := r.ensureReleaseDeletable(req.Context(), owner, name, version); err != nil {
			redirectManageError(w, req, err)
			return
		}
		if err := r.modules.DeleteRelease(req.Context(), owner, name, version); err != nil {
			redirectManageError(w, req, err)
			return
		}
		redirectManageResult(w, req, "/manage", "message", "version deleted")
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
	redirectManageResult(w, req, "/manage", "error", err.Error())
}

func redirectManageResult(w http.ResponseWriter, req *http.Request, fallback, key, message string) {
	target := manageReturnPath(req, fallback)
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	http.Redirect(w, req, target+separator+url.QueryEscape(key)+"="+url.QueryEscape(message), http.StatusFound)
}

func manageReturnPath(req *http.Request, fallback string) string {
	next := strings.TrimSpace(req.FormValue("next"))
	if next == "/manage/teams" || strings.HasPrefix(next, "/manage/teams/") {
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
