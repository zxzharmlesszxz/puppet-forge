package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
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
	canonicalPage, changed := normalizedListPage(page, pageSize, total)
	canonicalURL := managePageURLWithValues("/manage/modules", req.URL.Query(), canonicalPage, manageModuleListTarget)
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return
	}
	if changed {
		page = canonicalPage
		rows, total, err = r.loadManageModuleRows(req.Context(), principal, nil, query, page, pageSize)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	configureManageModuleRows(rows, csrfToken, "/manage/modules")
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
		Filter: newListFilter(
			"manage-module-filter", "catalog-query", "/manage/modules", manageModuleListTarget,
			"Filter modules", "q", "Filter by owner/name", query, "/manage/modules",
		),
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
	releaseCounts, err := r.modules.CountReleasesForModules(ctx, modules)
	if err != nil {
		return nil, 0, err
	}

	rows := make([]manageModuleRow, 0, len(modules))
	for _, module := range modules {
		canDelete := canDeleteInSpace(principal, module.Owner)
		rows = append(rows, manageModuleRow{
			Module:                module,
			ReleaseCount:          releaseCounts[module.Owner+"\x00"+module.Name],
			CanDelete:             canDelete,
			CanForceDeleteModule:  principal.CanAdmin,
			CanForceDeleteRelease: canDelete,
			CardID:                manageModuleCardID(module.Owner, module.Name),
			CardURL:               manageModuleCardURL(module.Owner, module.Name),
		})
	}
	return rows, total, nil
}

func (r *Router) loadManageModuleCard(ctx context.Context, principal auth.Principal, owner, name string) (manageModuleRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	module, err := r.modules.GetModule(ctx, owner, name)
	if err != nil {
		return manageModuleRow{}, err
	}
	versions, err := r.modules.ListReleases(ctx, owner, name)
	if err != nil {
		return manageModuleRow{}, err
	}
	activeReleases, err := r.modules.ListActiveReleasesForModules(ctx, time.Now().Add(-r.activeReleaseTTL), []domain.Module{module})
	if err != nil {
		return manageModuleRow{}, err
	}
	active := make(map[string]struct{}, len(activeReleases))
	for _, release := range activeReleases {
		active[release.Version] = struct{}{}
	}
	rows := make([]manageVersionRow, 0, len(versions))
	for _, version := range versions {
		_, inUse := active[version.Version]
		rows = append(rows, manageVersionRow{Version: version.Version, Active: inUse, Latest: version.Version == module.LatestVersion})
	}
	canDelete := canDeleteInSpace(principal, owner)
	return manageModuleRow{
		Module:                module,
		Versions:              rows,
		ReleaseCount:          len(rows),
		CanDelete:             canDelete,
		CanForceDeleteModule:  principal.CanAdmin,
		CanForceDeleteRelease: canDelete,
		VersionsLoaded:        true,
		CardID:                manageModuleCardID(owner, name),
		CardURL:               manageModuleCardURL(owner, name),
	}, nil
}

func configureManageModuleRows(rows []manageModuleRow, csrfToken, next string) {
	for i := range rows {
		row := &rows[i]
		row.CSRFToken = csrfToken
		row.Next = next
		row.ForceDeleteURL = manageForceDeleteURL(row.Module.Owner, row.Module.Name, "", next)
		for versionIndex := range row.Versions {
			version := &row.Versions[versionIndex]
			version.ForceDeleteURL = manageForceDeleteURL(row.Module.Owner, row.Module.Name, version.Version, next)
		}
		if next != "" && next != "/manage/modules" {
			row.CardURL += "?next=" + url.QueryEscape(next)
		}
	}
}

func manageForceDeleteURL(owner, name, version, next string) string {
	path := "/manage/modules/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if version == "" {
		path += "/force-delete"
	} else {
		path += "/versions/" + url.PathEscape(version) + "/force-delete"
	}
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	return path
}

func manageModuleCardID(owner, name string) string {
	return "item-" + owner + "-" + name
}

func manageModuleCardURL(owner, name string) string {
	return "/manage/modules/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/card"
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
		respondManageMutationHTTPError(w, req, http.StatusServiceUnavailable, errors.New("publish rate limit service is unavailable"))
		return
	}
	if !allowed {
		respondManageMutationHTTPError(w, req, http.StatusTooManyRequests, errors.New("too many publish attempts"))
		return
	}

	input, cleanup, err := readPublishInput(w, req, r.moduleUploadMax)
	if err != nil {
		r.audit(req, principal, "publish_module", "failure", auditReason(err))
		if isRequestTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		respondManageMutationError(w, req, err)
		return
	}
	defer cleanup()
	input.Replace = req.FormValue("replace") == "true"
	action, message := "publish_module", "module published"
	if input.Replace {
		action, message = "replace_module_release", "module release replaced"
	}
	requestedSpace := input.Owner
	var authorizationErr error
	r.prepareMutationAudit(req, principal, action)
	release, err := r.modules.PublishAuthorized(req.Context(), input, func(space string) error {
		requestedSpace = space
		switch {
		case !principal.CanPublishOwner(space):
			authorizationErr = errPublishSpaceForbidden
		case input.Replace && !canDeleteInSpace(principal, space):
			authorizationErr = errors.New("team administrator access is required to replace a release")
		}
		return authorizationErr
	})
	if err != nil {
		if authorizationErr != nil {
			r.audit(req, principal, action, "failure", "forbidden", "space", requestedSpace)
			respondManageMutationHTTPError(w, req, http.StatusForbidden, authorizationErr)
			return
		}
		r.audit(req, principal, action, "failure", auditReason(err), "space", requestedSpace)
		respondManageMutationServiceError(w, req, err)
		return
	}
	r.audit(req, principal, action, "success", "none", "space", release.Owner, "module", release.Owner+"/"+release.Name, "release", release.Version, "sha256", release.SHA256)
	respondManageMutationSuccess(w, req, "/manage/modules", message, manageModuleListTarget, "")
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
		respondManageMutationHTTPError(w, req, http.StatusForbidden, errors.New("global admin access required"))
		return
	}

	owner, name, err := parseUpstreamModuleFormValue(req.FormValue("module"))
	if err != nil {
		respondManageMutationServiceError(w, req, err)
		return
	}
	if err := r.modules.SyncUpstreamModule(req.Context(), owner, name); err != nil {
		r.audit(req, principal, "import_upstream_module", "failure", auditReason(err), "module", owner+"/"+name)
		respondManageMutationServiceError(w, req, err)
		return
	}
	r.audit(req, principal, "import_upstream_module", "success", "none", "module", owner+"/"+name)
	respondManageMutationSuccess(w, req, "/manage/modules", "upstream module added", manageModuleListTarget, "")
}

func (r *Router) manageModuleAction(w http.ResponseWriter, req *http.Request) {
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	trimmed := strings.Trim(strings.TrimPrefix(req.URL.Path, "/manage/modules/"), "/")
	parts := strings.Split(trimmed, "/")
	if req.Method == http.MethodGet && ((len(parts) == 3 && parts[2] == "force-delete") || (len(parts) == 5 && parts[2] == "versions" && parts[4] == "force-delete")) {
		r.renderManageForceDelete(w, req, principal, parts)
		return
	}
	if req.Method == http.MethodGet && len(parts) == 3 && parts[2] == "card" {
		owner, name := parts[0], parts[1]
		if !canViewManageSpace(principal, owner) {
			writeError(w, http.StatusForbidden, errors.New("module space access required"))
			return
		}
		row, err := r.loadManageModuleCard(req.Context(), principal, owner, name)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		csrfToken, err := r.ensureManageCSRFToken(w, req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rows := []manageModuleRow{row}
		configureManageModuleRows(rows, csrfToken, manageReturnPath(req, "/manage/modules"))
		row = rows[0]
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		executeHTMLTemplateNamed(w, managePageTemplate, "manage-module-card", row)
		return
	}
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !r.requireManageCSRF(w, req) {
		return
	}

	if len(parts) == 3 && parts[2] == "delete" {
		owner, name := parts[0], parts[1]
		if !canDeleteInSpace(principal, owner) {
			respondManageMutationHTTPError(w, req, http.StatusForbidden, errors.New("admin or team admin access required"))
			return
		}
		force, justification, err := forceDeleteParameters(req, owner+"/"+name)
		if err != nil {
			r.audit(req, principal, "force_delete_module", "failure", auditReason(err), "space", owner, "module", owner+"/"+name)
			respondManageMutationError(w, req, err)
			return
		}
		if force && !principal.CanAdmin {
			err := errors.New("global admin access required for force delete")
			r.audit(req, principal, "force_delete_module", "failure", "forbidden", "space", owner, "module", owner+"/"+name)
			respondManageMutationHTTPError(w, req, http.StatusForbidden, err)
			return
		}
		action := r.prepareDeleteMutationAudit(req, principal, owner, name, "", justification, force)
		if force {
			err = r.modules.DeleteModule(req.Context(), owner, name)
		} else {
			err = r.modules.DeleteModuleIfAllowed(req.Context(), owner, name, time.Now().Add(-r.activeReleaseTTL))
		}
		if err != nil {
			r.audit(req, principal, action, "failure", auditReason(err), "space", owner, "module", owner+"/"+name, "justification", justification)
			respondManageMutationServiceError(w, req, err)
			return
		}
		r.audit(req, principal, action, "success", "none", "space", owner, "module", owner+"/"+name, "justification", justification)
		respondManageMutationSuccess(w, req, "/manage/modules", "module deleted", "", manageModuleCardID(owner, name))
		return
	}
	if len(parts) == 5 && parts[2] == "versions" && parts[4] == "delete" {
		owner, name, version := parts[0], parts[1], parts[3]
		if !canDeleteInSpace(principal, owner) {
			respondManageMutationHTTPError(w, req, http.StatusForbidden, errors.New("admin or team admin access required"))
			return
		}
		expectedConfirmation := owner + "/" + name + "@" + version
		force, justification, err := forceDeleteParameters(req, expectedConfirmation)
		if err != nil {
			r.audit(req, principal, "force_delete_release", "failure", auditReason(err), "space", owner, "module", owner+"/"+name, "release", version)
			respondManageMutationError(w, req, err)
			return
		}
		action := r.prepareDeleteMutationAudit(req, principal, owner, name, version, justification, force)
		if force {
			err = r.modules.DeleteRelease(req.Context(), owner, name, version)
		} else {
			err = r.modules.DeleteReleaseIfAllowed(req.Context(), owner, name, version, time.Now().Add(-r.activeReleaseTTL))
		}
		if err != nil {
			r.audit(req, principal, action, "failure", auditReason(err), "space", owner, "module", owner+"/"+name, "release", version, "justification", justification)
			respondManageMutationServiceError(w, req, err)
			return
		}
		r.audit(req, principal, action, "success", "none", "space", owner, "module", owner+"/"+name, "release", version, "justification", justification)
		refreshTarget, removeTarget := manageModuleCardID(owner, name), ""
		if _, getErr := r.modules.GetModule(req.Context(), owner, name); errors.Is(getErr, store.ErrNotFound) {
			refreshTarget, removeTarget = "", manageModuleCardID(owner, name)
		}
		respondManageMutationSuccess(w, req, "/manage/modules", "version deleted", refreshTarget, removeTarget)
		return
	}

	writeError(w, http.StatusNotFound, errors.New("route not found"))
}

func (r *Router) renderManageForceDelete(w http.ResponseWriter, req *http.Request, principal auth.Principal, parts []string) {
	owner, name := parts[0], parts[1]
	isModule := len(parts) == 3
	if isModule {
		if !principal.CanAdmin {
			writeError(w, http.StatusForbidden, errors.New("global admin access required for force delete"))
			return
		}
	} else if !canDeleteInSpace(principal, owner) {
		writeError(w, http.StatusForbidden, errors.New("admin or team admin access required"))
		return
	}

	row, err := r.loadManageModuleCard(req.Context(), principal, owner, name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	next := manageReturnPath(req, "/manage/modules")
	data := manageForceDeleteData{
		ResourceKind: "module",
		Resource:     owner + "/" + name,
		Latest:       row.Module.LatestVersion,
		Action:       "/manage/modules/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/delete",
		Next:         next,
		ErrorNext:    manageForceDeleteURL(owner, name, "", next),
		Error:        req.URL.Query().Get("error"),
	}
	if isModule {
		data.Title = "Force delete module"
	} else {
		version := parts[3]
		var release *manageVersionRow
		for index := range row.Versions {
			if row.Versions[index].Version == version {
				release = &row.Versions[index]
				break
			}
		}
		if release == nil {
			writeServiceError(w, store.ErrNotFound)
			return
		}
		data.Title = "Force delete release"
		data.ResourceKind = "release"
		data.Resource += "@" + version
		data.Latest = ""
		data.Active = release.Active
		data.IsLatest = release.Latest
		data.Action = "/manage/modules/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/versions/" + url.PathEscape(version) + "/delete"
		data.ErrorNext = manageForceDeleteURL(owner, name, version, next)
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	page, team := manageForceDeleteNavigation(next)
	data.CSRFToken = csrfToken
	data.Navigation = r.manageNavigation(req, principal, csrfToken, page, team)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageForceDeleteTemplate, data)
}

func manageForceDeleteNavigation(next string) (string, string) {
	parsed, err := url.ParseRequestURI(next)
	if err == nil {
		team, section, _, ok := parseManageTeamPath(parsed.Path)
		if ok && section == manageTeamModulesSection {
			return "team-modules", team
		}
	}
	return "modules", ""
}

func forceDeleteParameters(req *http.Request, expectedConfirmation string) (bool, string, error) {
	force := strings.EqualFold(strings.TrimSpace(req.FormValue("force")), "true") || strings.EqualFold(strings.TrimSpace(req.Header.Get("X-Puppet-Forge-Force-Delete")), "true")
	if !force {
		return false, "", nil
	}
	justification := strings.TrimSpace(req.FormValue("reason"))
	if justification == "" {
		justification = strings.TrimSpace(req.Header.Get("X-Puppet-Forge-Delete-Reason"))
	}
	if justification == "" {
		return true, "", errors.New("force delete reason is required")
	}
	if len(justification) > 500 {
		return true, "", errors.New("force delete reason must not exceed 500 characters")
	}
	if strings.ContainsFunc(justification, unicode.IsControl) {
		return true, "", errors.New("force delete reason contains invalid control characters")
	}
	confirmation := strings.TrimSpace(req.FormValue("confirmation"))
	if confirmation == "" {
		confirmation = strings.TrimSpace(req.Header.Get("X-Puppet-Forge-Delete-Confirmation"))
	}
	if confirmation != expectedConfirmation {
		return true, "", errors.New("invalid force delete confirmation")
	}
	return true, justification, nil
}

func canViewManageSpace(principal auth.Principal, owner string) bool {
	if principal.CanAdmin || principal.CanPublishOwner(owner) {
		return true
	}
	return slices.Contains(manageableOwners(principal), owner)
}

func isAsyncManageMutation(req *http.Request) bool {
	return req.Header.Get("X-Puppet-Forge-Async-Mutation") == "true"
}

func respondManageMutationError(w http.ResponseWriter, req *http.Request, err error) {
	respondManageMutationHTTPError(w, req, http.StatusBadRequest, err)
}

func respondManageMutationHTTPError(w http.ResponseWriter, req *http.Request, status int, err error) {
	if isAsyncManageMutation(req) {
		writeError(w, status, err)
		return
	}
	redirectManageError(w, req, status, err)
}

func respondManageMutationServiceError(w http.ResponseWriter, req *http.Request, err error) {
	respondManageMutationHTTPError(w, req, serviceErrorStatus(err), err)
}

func respondManageMutationSuccess(w http.ResponseWriter, req *http.Request, fallback, message, refreshTarget, removeTarget string) {
	if isAsyncManageMutation(req) {
		if refreshTarget != "" {
			w.Header().Set("X-Puppet-Forge-Refresh", refreshTarget)
		}
		if removeTarget != "" {
			w.Header().Set("X-Puppet-Forge-Remove", removeTarget)
		}
		w.Header().Set("X-Puppet-Forge-Message", message)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	redirectManageResult(w, req, fallback, "message", message)
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

func manageErrorMessage(w http.ResponseWriter, status int, err error) string {
	logHTTPError(w, status, err)
	message := safeErrorMessage(status, err)
	if requestID := w.Header().Get(requestIDHeader); status >= http.StatusInternalServerError && requestID != "" {
		message += " (request_id: " + requestID + ")"
	}
	return message
}

func redirectManageError(w http.ResponseWriter, req *http.Request, status int, err error) {
	target := manageErrorReturnPath(req, "/manage/modules")
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	// #nosec G710 -- manageErrorReturnPath accepts only validated local force-delete paths or a fixed local fallback.
	http.Redirect(w, req, target+separator+"error="+url.QueryEscape(manageErrorMessage(w, status, err)), http.StatusFound)
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

func manageErrorReturnPath(req *http.Request, fallback string) string {
	next := strings.TrimSpace(req.FormValue("error_next"))
	parsed, err := url.ParseRequestURI(next)
	if err == nil && !parsed.IsAbs() && parsed.Host == "" && strings.HasPrefix(parsed.Path, "/manage/modules/") && strings.HasSuffix(parsed.Path, "/force-delete") {
		return next
	}
	return manageReturnPath(req, fallback)
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
