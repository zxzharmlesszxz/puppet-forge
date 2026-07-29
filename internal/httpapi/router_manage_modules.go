package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
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

	owners := manageableOwners(principal)
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	rows, err := r.loadManageModuleRows(req.Context(), principal, nil, query)
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
		Principal: principal,
		Owners:    owners,
		Modules:   rows,
		Message:   req.URL.Query().Get("message"),
		Error:     req.URL.Query().Get("error"),
		CSRFToken: csrfToken,
		Query:     query,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Router) loadManageModuleRows(ctx context.Context, principal auth.Principal, allowedOwners map[string]struct{}, query string) ([]manageModuleRow, error) {
	modules, err := r.modules.ListModules(ctx, 1000)
	if err != nil {
		return nil, err
	}
	allReleases, err := r.modules.ListAllReleases(ctx)
	if err != nil {
		return nil, err
	}
	releasesByModule := make(map[struct{ owner, name string }][]store.ReleaseSummary, len(modules))
	for _, rel := range allReleases {
		key := struct{ owner, name string }{rel.Owner, rel.Name}
		releasesByModule[key] = append(releasesByModule[key], rel)
	}
	activeReleases, err := r.modules.ListActiveReleases(ctx, time.Now().Add(-r.activeReleaseTTL))
	if err != nil {
		return nil, err
	}
	activeReleaseSet := make(map[struct{ owner, name, version string }]struct{}, len(activeReleases))
	for _, rel := range activeReleases {
		key := struct{ owner, name, version string }{rel.Owner, rel.Name, rel.Version}
		activeReleaseSet[key] = struct{}{}
	}

	queryLower := strings.ToLower(strings.TrimSpace(query))
	rows := make([]manageModuleRow, 0, len(modules))
	for _, module := range modules {
		if allowedOwners != nil {
			if _, allowed := allowedOwners[module.Owner]; !allowed {
				continue
			}
		} else if !principal.CanAdmin && !ownerAllowed(principal, module.Owner) {
			continue
		}
		if queryLower != "" && !strings.Contains(strings.ToLower(module.Owner+"/"+module.Name), queryLower) {
			continue
		}
		key := struct{ owner, name string }{module.Owner, module.Name}
		versions := releasesByModule[key]
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
	return rows, nil
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

func ownerAllowed(principal auth.Principal, owner string) bool {
	if principal.CanAdmin {
		return true
	}
	_, ok := principal.PublishOwners[owner]
	return ok
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
