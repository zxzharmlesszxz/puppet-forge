package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (r *Router) modulesCollection(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		if !r.rateLimiter.Allow(r.rateLimitKey(req, "module-search"), 600, time.Minute) {
			writeError(w, http.StatusTooManyRequests, errors.New("too many module search requests"))
			return
		}
		r.listModules(w, req)
	case http.MethodPost:
		r.publishModule(w, req)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (r *Router) publishSpaces(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	authorizer := r.currentAuthorizer(req.Context())
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return
	}
	principal, ok := authorizer.RequirePublishAny(w, req)
	if !ok || !r.requireActiveAccessToken(w, req, principal) {
		return
	}
	r.recordAccessTokenUsed(req.Context(), principal)
	spaces := make([]string, 0, len(principal.PublishOwners))
	for space := range principal.PublishOwners {
		spaces = append(spaces, space)
	}
	sort.Strings(spaces)
	writeJSON(w, http.StatusOK, map[string]any{"spaces": spaces})
}

func (r *Router) moduleItem(w http.ResponseWriter, req *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(req.URL.Path, "/api/v1/modules/"), "/")
	parts := strings.Split(trimmed, "/")

	if len(parts) == 2 && req.Method == http.MethodGet {
		r.getModule(w, req, parts[0], parts[1])
		return
	}
	if len(parts) == 2 && req.Method == http.MethodDelete {
		r.deleteModule(w, req, parts[0], parts[1])
		return
	}

	if len(parts) == 4 && parts[2] == "versions" && req.Method == http.MethodGet {
		r.getRelease(w, req, parts[0], parts[1], parts[3])
		return
	}
	if len(parts) == 4 && parts[2] == "versions" && req.Method == http.MethodDelete {
		r.deleteRelease(w, req, parts[0], parts[1], parts[3])
		return
	}

	if len(parts) == 5 && parts[2] == "versions" && parts[4] == "download" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		if !r.rateLimiter.Allow(r.rateLimitKey(req, "module-download"), 1200, time.Minute) {
			writeError(w, http.StatusTooManyRequests, errors.New("too many module download requests"))
			return
		}
		r.serveDownload(w, req, parts[0], parts[1], parts[3])
		return
	}

	writeError(w, http.StatusNotFound, errors.New("route not found"))
}

func (r *Router) modulePage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	trimmed := strings.Trim(strings.TrimPrefix(req.URL.Path, "/modules/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 6 && parts[2] == "versions" && parts[4] == "files" {
		if !r.requireReadAccess(w, req) {
			return
		}
		filePath := strings.Join(parts[5:], "/")
		if !validModuleFilePath(filePath) {
			writeError(w, http.StatusBadRequest, errors.New("invalid file path"))
			return
		}
		r.serveModuleFile(w, req, parts[0], parts[1], parts[3], filePath)
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}

	module, err := r.modules.GetModule(req.Context(), parts[0], parts[1])
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, errors.New("module not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	versions, err := r.modules.ListReleases(req.Context(), module.Owner, module.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	selectedVersion := req.URL.Query().Get("version")
	if selectedVersion == "" {
		selectedVersion = module.LatestVersion
	}

	var release domain.Release
	if selectedVersion != "" {
		release, err = r.modules.GetRelease(req.Context(), module.Owner, module.Name, selectedVersion)
		if err != nil {
			writeServiceError(w, err)
			return
		}
	}

	navigation := r.publicNavigation(
		req,
		"/manage",
		"Manage",
		publicBreadcrumb{Label: "Modules", URL: "/"},
		publicBreadcrumb{Label: module.Owner, URL: "/?owner=" + url.QueryEscape(module.Owner)},
		publicBreadcrumb{Label: module.Name, URL: req.URL.Path},
	)
	navigation.Versions = versions
	navigation.SelectedVersion = selectedVersion

	page := modulePageData{
		Navigation:      navigation,
		Module:          module,
		Release:         release,
		ReadmeHTML:      renderMarkdown(release.Readme, readmeBaseHref(module.Owner, module.Name, release.Version)),
		DownloadPath:    downloadPath(module.Owner, module.Name, release.Version),
		IsUpstream:      release.Source == "upstream",
		PublicBaseURL:   httputil.ExternalBaseURL(req, r.publicBaseURL),
		ModuleInstallID: moduleSlug(module.Owner, module.Name),
		ReadTokenHint:   "Bearer <READ_TOKEN>", // #nosec G101 -- documentation placeholder, not a credential.
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, modulePageTemplate, page)
}

func (r *Router) serveModuleFile(w http.ResponseWriter, req *http.Request, owner, name, version, filePath string) {
	object, err := r.modules.ReadReleaseFile(req.Context(), owner, name, version, filePath)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}

	if object.ContentType != "" {
		w.Header().Set("Content-Type", object.ContentType)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(object.Body)
}

func validModuleFilePath(filePath string) bool {
	trimmed := strings.TrimPrefix(filePath, "/")
	if trimmed == "" || strings.Contains(trimmed, "\x00") || strings.Contains(trimmed, "//") {
		return false
	}
	cleanPath := path.Clean(trimmed)
	return cleanPath != "." && cleanPath != ".." && !strings.HasPrefix(cleanPath, "../") && cleanPath == trimmed
}

func (r *Router) listModules(w http.ResponseWriter, req *http.Request) {
	if !r.requireReadAccess(w, req) {
		return
	}

	limit := 20
	if raw := req.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > store.MaxModulePageSize {
			writeError(w, http.StatusBadRequest, errors.New("invalid limit"))
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := req.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > store.MaxModulePageOffset {
			writeError(w, http.StatusBadRequest, errors.New("invalid offset"))
			return
		}
		offset = parsed
	}

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	modules, total, err := r.modules.ListModulesPage(ctx, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":  modules,
		"limit":  limit,
		"offset": offset,
		"total":  total,
	})
}

func writeStoreResponse[T any](w http.ResponseWriter, data T, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (r *Router) getModule(w http.ResponseWriter, req *http.Request, owner, name string) {
	if !r.requireReadAccess(w, req) {
		return
	}
	module, err := r.modules.GetModule(req.Context(), owner, name)
	writeStoreResponse(w, module, err)
}

func (r *Router) requireDeleteAccess(w http.ResponseWriter, req *http.Request, owner string) bool {
	authorizer := r.currentAuthorizer(req.Context())
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return false
	}
	principal, ok := authorizer.RequireDelete(w, req, owner)
	if ok && r.requireActiveAccessToken(w, req, principal) {
		r.recordAccessTokenUsed(req.Context(), principal)
		return true
	}
	return false
}

func writeDeleteResponse(w http.ResponseWriter, entityType string, data map[string]string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		metrics.ObserveDelete(entityType, err)
		writeError(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, service.ErrProtectedDelete) {
		metrics.ObserveDelete(entityType, err)
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		metrics.ObserveDelete(entityType, err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	metrics.ObserveDelete(entityType, nil)
	writeJSON(w, http.StatusOK, data)
}

func (r *Router) deleteModule(w http.ResponseWriter, req *http.Request, owner, name string) {
	if !r.requireDeleteAccess(w, req, owner) {
		return
	}
	err := r.modules.DeleteModuleIfAllowed(req.Context(), owner, name, time.Now().Add(-r.activeReleaseTTL))
	writeDeleteResponse(w, "module", map[string]string{
		"status": "deleted",
		"owner":  owner,
		"name":   name,
	}, err)
}

func (r *Router) getRelease(w http.ResponseWriter, req *http.Request, owner, name, version string) {
	if !r.requireReadAccess(w, req) {
		return
	}
	release, err := r.modules.GetRelease(req.Context(), owner, name, version)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, newReleaseAPIResponse(release))
}

func (r *Router) deleteRelease(w http.ResponseWriter, req *http.Request, owner, name, version string) {
	if !r.requireDeleteAccess(w, req, owner) {
		return
	}
	err := r.modules.DeleteReleaseIfAllowed(req.Context(), owner, name, version, time.Now().Add(-r.activeReleaseTTL))
	writeDeleteResponse(w, "release", map[string]string{
		"status":  "deleted",
		"owner":   owner,
		"name":    name,
		"version": version,
	}, err)
}

func (r *Router) serveDownload(w http.ResponseWriter, req *http.Request, owner, name, version string) {
	if !r.requireReadAccess(w, req) {
		return
	}

	release, err := r.modules.GetRelease(req.Context(), owner, name, version)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	r.markReleaseUsed(req.Context(), owner, name, version)

	if release.Source == "upstream" {
		localPath := "/v3/files/" + releaseV3FileName(release)
		// #nosec G710 -- localPath is an application-relative path built from a validated release identity.
		http.Redirect(w, req, localPath, http.StatusFound)
		return
	}

	r.writeReleaseArchive(w, req, release)
}

func (r *Router) markReleaseUsed(ctx context.Context, owner, name, version string) {
	if err := r.modules.MarkReleaseUsed(ctx, owner, name, version); err != nil {
		slog.Warn("mark release used failed", "err", err, "owner", owner, "name", name, "version", version)
	}
}

func (r *Router) publishModule(w http.ResponseWriter, req *http.Request) {
	allowed, rateLimitErr := r.allowSharedRateLimit(req, "publish", 60, time.Minute)
	if rateLimitErr != nil {
		r.audit(req, auth.Principal{}, "publish_module", "failure", "rate_limit_unavailable")
		writeError(w, http.StatusServiceUnavailable, errors.New("publish rate limit service is unavailable"))
		return
	}
	if !allowed {
		r.audit(req, auth.Principal{}, "publish_module", "failure", "rate_limited")
		writeError(w, http.StatusTooManyRequests, errors.New("too many publish attempts"))
		return
	}
	authorizer := r.currentAuthorizer(req.Context())
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return
	}
	principal, ok := authorizer.RequirePublishAny(w, req)
	if !ok || !r.requireActiveAccessToken(w, req, principal) {
		r.audit(req, auth.Principal{}, "publish_module", "failure", "unauthorized")
		return
	}
	r.recordAccessTokenUsed(req.Context(), principal)

	input, cleanup, err := readPublishInput(w, req, r.moduleUploadMax)
	if err != nil {
		r.audit(req, principal, "publish_module", "failure", auditReason(err))
		if isRequestTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer cleanup()
	if authorizer.Enabled() && !principal.CanPublishOwner(input.Owner) {
		r.audit(req, principal, "publish_module", "failure", "forbidden", "space", input.Owner)
		writeError(w, http.StatusForbidden, errors.New("token is not allowed to publish to this space"))
		return
	}
	release, err := r.modules.Publish(req.Context(), input)
	if err != nil {
		r.audit(req, principal, "publish_module", "failure", auditReason(err), "space", input.Owner)
		writeServiceError(w, err)
		return
	}

	r.audit(req, principal, "publish_module", "success", "none", "space", release.Owner, "module", release.Owner+"/"+release.Name, "release", release.Version, "sha256", release.SHA256)
	writeJSON(w, http.StatusCreated, newReleaseAPIResponse(release))
}

type releaseAPIResponse struct {
	ID          string         `json:"id"`
	ModuleID    string         `json:"module_id"`
	Owner       string         `json:"owner"`
	Name        string         `json:"name"`
	Source      string         `json:"source,omitempty"`
	Version     string         `json:"version"`
	Description string         `json:"description,omitempty"`
	Readme      string         `json:"readme,omitempty"`
	FileName    string         `json:"file_name"`
	ContentType string         `json:"content_type"`
	SizeBytes   int64          `json:"size_bytes"`
	MD5         string         `json:"md5"`
	SHA256      string         `json:"sha256"`
	DownloadURL string         `json:"download_url"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

func newReleaseAPIResponse(release domain.Release) releaseAPIResponse {
	return releaseAPIResponse{
		ID:          release.ID,
		ModuleID:    release.ModuleID,
		Owner:       release.Owner,
		Name:        release.Name,
		Source:      release.Source,
		Version:     release.Version,
		Description: release.Description,
		Readme:      release.Readme,
		FileName:    release.FileName,
		ContentType: release.ContentType,
		SizeBytes:   release.SizeBytes,
		MD5:         release.MD5,
		SHA256:      release.SHA256,
		DownloadURL: fmt.Sprintf(
			"/api/v1/modules/%s/%s/versions/%s/download",
			url.PathEscape(release.Owner),
			url.PathEscape(release.Name),
			url.PathEscape(release.Version),
		),
		Metadata:  release.Metadata,
		CreatedAt: release.CreatedAt,
	}
}

func readPublishInput(w http.ResponseWriter, req *http.Request, maxBytes int64) (domain.PublishModuleInput, func(), error) {
	if maxBytes > 0 {
		requestLimit := maxBytes
		const multipartOverheadLimit int64 = 1 << 20
		if requestLimit <= math.MaxInt64-multipartOverheadLimit {
			requestLimit += multipartOverheadLimit
		} else {
			requestLimit = math.MaxInt64
		}
		req.Body = http.MaxBytesReader(w, req.Body, requestLimit)
	}
	// #nosec G120 -- MaxBytesReader above bounds the archive plus controlled multipart overhead.
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		if req.MultipartForm != nil {
			_ = req.MultipartForm.RemoveAll()
		}
		return domain.PublishModuleInput{}, nil, fmt.Errorf("parse multipart form: %w", err)
	}
	removeForm := func() {
		_ = req.MultipartForm.RemoveAll()
	}
	for _, field := range []string{"owner", "name", "version", "summary", "description", "metadata"} {
		if _, exists := req.MultipartForm.Value[field]; exists {
			removeForm()
			return domain.PublishModuleInput{}, nil, fmt.Errorf("manual field %q is not allowed; use space and file only", field)
		}
	}
	space := strings.TrimSpace(req.FormValue("space"))
	if space == "" {
		removeForm()
		return domain.PublishModuleInput{}, nil, errors.New("space is required")
	}

	file, header, err := req.FormFile("file")
	if err != nil {
		removeForm()
		if errors.Is(err, http.ErrMissingFile) {
			return domain.PublishModuleInput{}, nil, errors.New("artifact file is required")
		}
		return domain.PublishModuleInput{}, nil, fmt.Errorf("read file: %w", err)
	}
	if maxBytes > 0 && header.Size > maxBytes {
		_ = file.Close()
		removeForm()
		return domain.PublishModuleInput{}, nil, requestTooLargeError{limit: maxBytes}
	}
	cleanup := func() {
		_ = file.Close()
		removeForm()
	}

	input := domain.PublishModuleInput{
		Owner:       space,
		FileName:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		File:        file,
		SizeBytes:   header.Size,
	}

	if input.ContentType == "" {
		input.ContentType = "application/gzip"
	}
	return input, cleanup, nil
}

func isRequestTooLarge(err error) bool {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return true
	}
	var uploadErr requestTooLargeError
	return errors.As(err, &uploadErr)
}
