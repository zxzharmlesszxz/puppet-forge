package httpapi

import (
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

	modules, err := r.modules.ListModules(req.Context(), 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	allReleases, err := r.modules.ListAllReleases(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	releasesByModule := make(map[struct{ owner, name string }][]store.ReleaseSummary, len(modules))
	for _, rel := range allReleases {
		key := struct{ owner, name string }{rel.Owner, rel.Name}
		releasesByModule[key] = append(releasesByModule[key], rel)
	}
	activeSince := time.Now().Add(-r.activeReleaseTTL)
	activeReleases, err := r.modules.ListActiveReleases(req.Context(), activeSince)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	activeReleaseSet := make(map[struct{ owner, name, version string }]struct{}, len(activeReleases))
	for _, rel := range activeReleases {
		key := struct{ owner, name, version string }{rel.Owner, rel.Name, rel.Version}
		activeReleaseSet[key] = struct{}{}
	}

	owners := manageableOwners(principal)
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	queryLower := strings.ToLower(query)
	rows := make([]manageModuleRow, 0, len(modules))
	for _, module := range modules {
		if !principal.CanAdmin && !ownerAllowed(principal, module.Owner) {
			continue
		}
		if query != "" {
			ownerName := strings.ToLower(module.Owner + "/" + module.Name)
			if !strings.Contains(ownerName, queryLower) {
				continue
			}
		}
		key := struct{ owner, name string }{module.Owner, module.Name}
		versions := releasesByModule[key]
		versionRows := make([]manageVersionRow, 0, len(versions))
		hasProtectedRelease := false
		for _, version := range versions {
			_, active := activeReleaseSet[struct{ owner, name, version string }{module.Owner, module.Name, version.Version}]
			latest := version.Version == module.LatestVersion
			if latest || active {
				hasProtectedRelease = true
			}
			versionRows = append(versionRows, manageVersionRow{
				Version: version.Version,
				Active:  active,
				Latest:  latest,
			})
		}
		rows = append(rows, manageModuleRow{
			Module:    module,
			Versions:  versionRows,
			CanDelete: canDeleteInSpace(principal, module.Owner) && !hasProtectedRelease,
		})
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
	input, err = r.modules.NormalizePublishInput(input)
	if err != nil {
		redirectManageError(w, req, err)
		return
	}
	if !principal.CanPublish || !ownerAllowed(principal, input.Owner) {
		redirectManageError(w, req, errors.New("token is not allowed to publish to this space"))
		return
	}

	if _, err := r.modules.Publish(req.Context(), input); err != nil {
		redirectManageError(w, req, err)
		return
	}
	http.Redirect(w, req, "/manage?message=module+published", http.StatusFound)
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
		http.Redirect(w, req, "/manage?message=module+deleted", http.StatusFound)
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
		http.Redirect(w, req, "/manage?message=version+deleted", http.StatusFound)
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
	http.Redirect(w, req, "/manage?error="+url.QueryEscape(err.Error()), http.StatusFound)
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
	if principal.CanAdmin {
		return nil
	}
	owners := make([]string, 0, len(principal.PublishOwners))
	for owner := range principal.PublishOwners {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	return owners
}
