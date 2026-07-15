package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"puppet-forge/internal/domain"
	"puppet-forge/internal/httputil"
	"puppet-forge/internal/metrics"
	"puppet-forge/internal/store"
)

func (r *Router) modulesCollection(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		r.listModules(w, req)
	case http.MethodPost:
		r.publishModule(w, req)
	default:
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
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

	if len(parts) == 5 && parts[2] == "versions" && parts[4] == "download" && req.Method == http.MethodGet {
		r.redirectDownload(w, req, parts[0], parts[1], parts[3])
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
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	page := modulePageData{
		Module:          module,
		Release:         release,
		Versions:        versions,
		SelectedVersion: selectedVersion,
		ReadmeHTML:      renderMarkdown(release.Readme, readmeBaseHref(module.Owner, module.Name, release.Version)),
		DownloadPath:    downloadPath(module.Owner, module.Name, release.Version),
		IsUpstream:      release.Source == "upstream",
		PublicBaseURL:   httputil.ExternalBaseURL(req, r.publicBaseURL),
		ModuleInstallID: moduleSlug(module.Owner, module.Name),
		ReadTokenHint:   "Bearer <READ_TOKEN>",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := modulePageTemplate.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Router) serveModuleFile(w http.ResponseWriter, req *http.Request, owner, name, version, filePath string) {
	object, err := r.modules.ReadReleaseFile(req.Context(), owner, name, version, filePath)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
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
		if err != nil || parsed <= 0 || parsed > 100 {
			writeError(w, http.StatusBadRequest, errors.New("invalid limit"))
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := req.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, errors.New("invalid offset"))
			return
		}
		offset = parsed
	}

	modules, total, err := r.modules.ListModulesPage(req.Context(), limit, offset)
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
	authorizer := r.currentAuthorizer()
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return false
	}
	_, ok := authorizer.RequireDelete(w, req, owner)
	return ok
}

func writeDeleteResponse(w http.ResponseWriter, owner, entityType string, data map[string]string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		metrics.ObserveDelete(entityType, owner, err)
		writeError(w, http.StatusNotFound, err)
		return
	}
	if _, ok := errors.AsType[protectedDeleteError](err); ok {
		metrics.ObserveDelete(entityType, owner, err)
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		metrics.ObserveDelete(entityType, owner, err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	metrics.ObserveDelete(entityType, owner, nil)
	writeJSON(w, http.StatusOK, data)
}

func (r *Router) deleteModule(w http.ResponseWriter, req *http.Request, owner, name string) {
	if !r.requireDeleteAccess(w, req, owner) {
		return
	}
	err := r.ensureModuleDeletable(req.Context(), owner, name)
	if err == nil {
		err = r.modules.DeleteModule(req.Context(), owner, name)
	}
	writeDeleteResponse(w, owner, "module", map[string]string{
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
	writeStoreResponse(w, release, err)
}

func (r *Router) deleteRelease(w http.ResponseWriter, req *http.Request, owner, name, version string) {
	if !r.requireDeleteAccess(w, req, owner) {
		return
	}
	err := r.ensureReleaseDeletable(req.Context(), owner, name, version)
	if err == nil {
		err = r.modules.DeleteRelease(req.Context(), owner, name, version)
	}
	writeDeleteResponse(w, owner, "release", map[string]string{
		"status":  "deleted",
		"owner":   owner,
		"name":    name,
		"version": version,
	}, err)
}

func (r *Router) ensureModuleDeletable(ctx context.Context, owner, name string) error {
	module, err := r.modules.GetModule(ctx, owner, name)
	if err != nil {
		return err
	}

	versions, err := r.modules.ListReleases(ctx, owner, name)
	if err != nil {
		return err
	}
	for _, version := range versions {
		if err := r.protectedReleaseDeleteError(ctx, module, version.Version); err != nil {
			return protectedDeleteError{message: "module contains " + err.Error()}
		}
	}
	return nil
}

func (r *Router) ensureReleaseDeletable(ctx context.Context, owner, name, version string) error {
	module, err := r.modules.GetModule(ctx, owner, name)
	if err != nil {
		return err
	}
	if _, err := r.modules.GetRelease(ctx, owner, name, version); err != nil {
		return err
	}
	return r.protectedReleaseDeleteError(ctx, module, version)
}

func (r *Router) protectedReleaseDeleteError(ctx context.Context, module domain.Module, version string) error {
	if version != "" && module.LatestVersion == version {
		return protectedDeleteError{message: fmt.Sprintf("latest release %s/%s %s cannot be deleted", module.Owner, module.Name, version)}
	}
	activeSince := time.Now().Add(-r.activeReleaseTTL)
	active, err := r.modules.IsReleaseActive(ctx, module.Owner, module.Name, version, activeSince)
	if err != nil {
		return err
	}
	if active {
		return protectedDeleteError{message: fmt.Sprintf("active release %s/%s %s cannot be deleted", module.Owner, module.Name, version)}
	}
	return nil
}

func (r *Router) redirectDownload(w http.ResponseWriter, req *http.Request, owner, name, version string) {
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
		http.Redirect(w, req, localPath, http.StatusFound)
		return
	}

	http.Redirect(w, req, release.DownloadURL, http.StatusFound)
}

func (r *Router) markReleaseUsed(ctx context.Context, owner, name, version string) {
	if err := r.modules.MarkReleaseUsed(ctx, owner, name, version); err != nil {
		slog.Warn("mark release used failed", "err", err, "owner", owner, "name", name, "version", version)
	}
}

func (r *Router) publishModule(w http.ResponseWriter, req *http.Request) {
	if !r.rateLimiter.Allow(rateLimitKey(req, "publish"), 60, time.Minute) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many publish attempts"))
		return
	}
	authorizer := r.currentAuthorizer()
	if authorizer == nil {
		writeError(w, http.StatusInternalServerError, errors.New("authorizer is not configured"))
		return
	}
	principal, ok := authorizer.RequirePublishAny(w, req)
	if !ok {
		return
	}

	input, err := readPublishInput(w, req, r.moduleUploadMax)
	if err != nil {
		if isRequestTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	input, err = r.modules.NormalizePublishInput(input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if authorizer.Enabled() {
		if _, allowed := principal.PublishOwners[input.Owner]; !allowed {
			writeError(w, http.StatusForbidden, errors.New("token is not allowed to publish to this space"))
			return
		}
	}

	release, err := r.modules.Publish(req.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusCreated, release)
}

func readPublishInput(w http.ResponseWriter, req *http.Request, maxBytes int64) (domain.PublishModuleInput, error) {
	if maxBytes > 0 {
		req.Body = http.MaxBytesReader(w, req.Body, maxBytes)
	}
	if err := req.ParseMultipartForm(64 << 20); err != nil {
		return domain.PublishModuleInput{}, fmt.Errorf("parse multipart form: %w", err)
	}

	file, header, err := req.FormFile("file")
	if err != nil {
		return domain.PublishModuleInput{}, fmt.Errorf("read file: %w", err)
	}
	if maxBytes > 0 && header.Size > maxBytes {
		_ = file.Close()
		return domain.PublishModuleInput{}, requestTooLargeError{limit: maxBytes}
	}

	body, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil {
		return domain.PublishModuleInput{}, fmt.Errorf("read file content: %w", err)
	}
	if closeErr != nil {
		return domain.PublishModuleInput{}, fmt.Errorf("close file: %w", closeErr)
	}

	metadata, err := parseMetadata(req.MultipartForm.Value["metadata"])
	if err != nil {
		return domain.PublishModuleInput{}, err
	}

	input := domain.PublishModuleInput{
		Owner:       req.FormValue("owner"),
		Name:        req.FormValue("name"),
		Version:     req.FormValue("version"),
		Description: req.FormValue("description"),
		FileName:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		FileBytes:   body,
		Metadata:    metadata,
	}

	if input.ContentType == "" {
		input.ContentType = "application/gzip"
	}
	return input, nil
}

func isRequestTooLarge(err error) bool {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return true
	}
	var uploadErr requestTooLargeError
	return errors.As(err, &uploadErr)
}

func parseMetadata(values []string) (map[string]any, error) {
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return map[string]any{}, nil
	}

	var metadata map[string]any
	if err := json.Unmarshal([]byte(values[0]), &metadata); err != nil {
		return nil, fmt.Errorf("parse metadata json: %w", err)
	}

	return metadata, nil
}
