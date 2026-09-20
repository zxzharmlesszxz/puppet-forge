package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (r *Router) v3Handler(w http.ResponseWriter, req *http.Request) {
	if r.serveLocalV3Response(w, req) {
		return
	}
	if r.forgeProxy == nil {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	r.forgeProxy.ServeHTTP(w, req)
}

func (r *Router) serveLocalV3Response(w http.ResponseWriter, req *http.Request) bool {
	trimmed := strings.Trim(strings.TrimPrefix(req.URL.Path, "/v3/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 && req.Method == http.MethodGet {
		switch parts[0] {
		case "modules":
			r.serveLocalV3Modules(w, req)
			return true
		case "releases":
			r.serveLocalV3Releases(w, req)
			return true
		}
	}
	if len(parts) != 2 {
		return false
	}

	switch parts[0] {
	case "modules":
		if req.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return true
		}
		return r.serveLocalV3Module(w, req, parts[1])
	case "releases":
		if req.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return true
		}
		return r.serveLocalV3Release(w, req, parts[1])
	case "files":
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return true
		}
		return r.serveLocalV3File(w, req, parts[1])
	default:
		return false
	}
}

type v3ReleaseRef struct {
	Slug    string `json:"slug"`
	Version string `json:"version"`
}

func (r *Router) serveLocalV3Modules(w http.ResponseWriter, req *http.Request) {
	limit, offset, ok := v3Pagination(w, req)
	if !ok {
		return
	}
	modules, total, err := r.modules.ListModulesPageFiltered(req.Context(), nil, req.URL.Query().Get("query"), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	releasesByModule, err := r.modules.ListReleasesForModules(req.Context(), modules)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	results := make([]map[string]any, 0, len(modules))
	for _, module := range modules {
		versions := releasesByModule[module.Owner+"\x00"+module.Name]
		releaseRefs := make([]v3ReleaseRef, 0, len(versions))
		for _, version := range versions {
			releaseRefs = append(releaseRefs, v3ReleaseRef{Slug: releaseSlug(module.Owner, module.Name, version.Version), Version: version.Version})
		}

		var currentRelease any
		if module.LatestVersion != "" {
			release, getErr := r.modules.GetStoredRelease(req.Context(), module.Owner, module.Name, module.LatestVersion)
			if getErr != nil {
				writeServiceError(w, getErr)
				return
			}
			currentRelease = v3ReleaseResponse(req, release)
		}
		results = append(results, map[string]any{
			"uri":             "/v3/modules/" + moduleSlug(module.Owner, module.Name),
			"slug":            moduleSlug(module.Owner, module.Name),
			"name":            module.Name,
			"owner":           v3OwnerResponse(module.Owner),
			"current_release": currentRelease,
			"releases":        releaseRefs,
		})
	}
	writeV3Collection(w, req, results, limit, offset, total)
}

func (r *Router) serveLocalV3Releases(w http.ResponseWriter, req *http.Request) {
	limit, offset, ok := v3Pagination(w, req)
	if !ok {
		return
	}
	moduleFilter := strings.ReplaceAll(strings.TrimSpace(req.URL.Query().Get("module")), "/", "-")
	versionFilter := strings.TrimSpace(req.URL.Query().Get("version"))
	summaries, err := r.modules.ListAllReleases(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	filtered := summaries[:0]
	for _, summary := range summaries {
		if moduleFilter != "" && moduleSlug(summary.Owner, summary.Name) != moduleFilter {
			continue
		}
		if versionFilter != "" && summary.Version != versionFilter {
			continue
		}
		filtered = append(filtered, summary)
	}
	total := len(filtered)
	start := min(offset, total)
	end := min(start+limit, total)
	results := make([]map[string]any, 0, end-start)
	for _, summary := range filtered[start:end] {
		release, getErr := r.modules.GetStoredRelease(req.Context(), summary.Owner, summary.Name, summary.Version)
		if getErr != nil {
			writeServiceError(w, getErr)
			return
		}
		results = append(results, v3ReleaseResponse(req, release))
	}
	writeV3Collection(w, req, results, limit, offset, total)
}

func v3OwnerResponse(owner string) map[string]any {
	return map[string]any{
		"uri":      "/v3/users/" + owner,
		"slug":     owner,
		"username": owner,
	}
}

func v3ReleaseResponse(req *http.Request, release domain.Release) map[string]any {
	response := map[string]any{
		"uri":       "/v3/releases/" + releaseSlug(release.Owner, release.Name, release.Version),
		"slug":      releaseSlug(release.Owner, release.Name, release.Version),
		"module":    map[string]any{"uri": "/v3/modules/" + moduleSlug(release.Owner, release.Name), "slug": moduleSlug(release.Owner, release.Name), "name": release.Name, "owner": v3OwnerResponse(release.Owner)},
		"version":   release.Version,
		"metadata":  release.Metadata,
		"tags":      v3MetadataStrings(release.Metadata, "tags"),
		"file_uri":  releaseV3FileURI(req, release),
		"file_name": releaseV3FileName(release),
		"file_size": release.SizeBytes,
	}
	if release.MD5 != "" {
		response["file_md5"] = release.MD5
	}
	if release.SHA256 != "" {
		response["file_sha256"] = release.SHA256
	}
	return response
}

func v3MetadataStrings(metadata map[string]any, key string) []string {
	values, ok := metadata[key].([]any)
	if !ok {
		if typedValues, valid := metadata[key].([]string); valid {
			return typedValues
		}
		return []string{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, valid := value.(string); valid {
			result = append(result, text)
		}
	}
	return result
}

func v3Pagination(w http.ResponseWriter, req *http.Request) (int, int, bool) {
	limit, ok := v3PaginationValue(w, req, "limit", 20, 1, store.MaxModulePageSize)
	if !ok {
		return 0, 0, false
	}
	offset, ok := v3PaginationValue(w, req, "offset", 0, 0, store.MaxModulePageOffset)
	return limit, offset, ok
}

func v3PaginationValue(w http.ResponseWriter, req *http.Request, name string, fallback, minimum, maximum int) (int, bool) {
	raw := req.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		writeError(w, http.StatusBadRequest, errors.New("invalid "+name))
		return 0, false
	}
	return value, true
}

func writeV3Collection[T any](w http.ResponseWriter, req *http.Request, results []T, limit, offset, total int) {
	writeJSON(w, http.StatusOK, map[string]any{
		"pagination": v3PaginationResponse(req, limit, offset, total),
		"results":    results,
	})
}

func v3PaginationResponse(req *http.Request, limit, offset, total int) map[string]any {
	pageURL := func(target int) string {
		query := req.URL.Query()
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(target))
		return httputil.ExternalBaseURL(req, "") + req.URL.Path + "?" + query.Encode()
	}
	lastOffset := 0
	if total > 0 {
		lastOffset = (total - 1) / limit * limit
	}
	pagination := map[string]any{
		"limit":    limit,
		"offset":   offset,
		"total":    total,
		"first":    pageURL(0),
		"current":  pageURL(offset),
		"last":     pageURL(lastOffset),
		"previous": nil,
		"next":     nil,
	}
	if offset > 0 {
		pagination["previous"] = pageURL(max(0, offset-limit))
	}
	if offset+limit < total {
		pagination["next"] = pageURL(offset + limit)
	}
	return pagination
}

func (r *Router) serveLocalV3Module(w http.ResponseWriter, req *http.Request, slug string) bool {
	module, ok, err := r.findModuleBySlug(req.Context(), slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return true
	}
	if !ok {
		return false
	}

	versions, err := r.modules.ListReleases(req.Context(), module.Owner, module.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return true
	}

	type releaseRef struct {
		Slug    string `json:"slug"`
		Version string `json:"version"`
	}

	releases := make([]releaseRef, 0, len(versions))
	for _, version := range versions {
		releases = append(releases, releaseRef{
			Slug:    releaseSlug(module.Owner, module.Name, version.Version),
			Version: version.Version,
		})
	}
	var currentRelease *releaseRef
	if module.LatestVersion != "" {
		currentRelease = &releaseRef{
			Slug:    releaseSlug(module.Owner, module.Name, module.LatestVersion),
			Version: module.LatestVersion,
		}
	}

	response := map[string]any{
		"slug":            slug,
		"owner":           module.Owner,
		"name":            module.Name,
		"current_release": currentRelease,
		"releases":        releases,
	}
	writeJSON(w, http.StatusOK, response)
	return true
}

func (r *Router) serveLocalV3Release(w http.ResponseWriter, req *http.Request, slug string) bool {
	release, ok, err := r.findReleaseBySlug(req.Context(), slug)
	if err != nil {
		writeServiceError(w, err)
		return true
	}
	if !ok {
		return false
	}
	r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
	verifiedRelease, err := r.modules.EnsureReleaseChecksums(req.Context(), release)
	if err != nil {
		if release.Source == "upstream" {
			// A successful local v3 response must include checksums from the exact
			// artifact that its file_uri serves. Let the Forge proxy return the
			// authoritative upstream response when local materialization is incomplete.
			return false
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return true
		}
		writeError(w, http.StatusInternalServerError, err)
		return true
	}
	release = verifiedRelease

	response := map[string]any{
		"slug":        releaseSlug(release.Owner, release.Name, release.Version),
		"version":     release.Version,
		"file_uri":    releaseV3FileURI(req, release),
		"file_name":   releaseV3FileName(release),
		"file_size":   release.SizeBytes,
		"readme":      release.Readme,
		"description": release.Description,
		"metadata":    release.Metadata,
	}
	if release.MD5 != "" {
		response["file_md5"] = release.MD5
	}
	if release.SHA256 != "" {
		response["file_sha256"] = release.SHA256
	}
	writeJSON(w, http.StatusOK, response)
	return true
}

func (r *Router) serveLocalV3File(w http.ResponseWriter, req *http.Request, filename string) bool {
	release, ok, err := r.findReleaseByFileName(req.Context(), filename)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return true
	}
	if !ok {
		return false
	}
	if release.Source == "upstream" && (release.MD5 == "" || release.SHA256 == "" || release.SizeBytes <= 0 || release.StoragePath == "") {
		r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
		if req.Method == http.MethodGet {
			r.markReleaseConsumerUsed(req.Context(), release.Owner, release.Name, release.Version)
		}
		return false
	}

	r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
	if req.Method == http.MethodGet {
		r.markReleaseConsumerUsed(req.Context(), release.Owner, release.Name, release.Version)
	}
	r.writeReleaseArchive(w, req, release)
	return true
}

func (r *Router) findModuleBySlug(ctx context.Context, slug string) (domain.Module, bool, error) {
	module, err := r.modules.GetModuleBySlug(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Module{}, false, nil
	}
	return module, err == nil, err
}

func (r *Router) findReleaseBySlug(ctx context.Context, slug string) (domain.Release, bool, error) {
	release, err := r.modules.GetReleaseBySlug(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Release{}, false, nil
	}
	if errors.Is(err, service.ErrUpstreamHydration) && !errors.Is(err, service.ErrUpstreamRestore) {
		return domain.Release{}, false, nil
	}
	return release, err == nil, err
}

func (r *Router) findReleaseByFileName(ctx context.Context, filename string) (domain.Release, bool, error) {
	if !strings.HasSuffix(filename, ".tar.gz") {
		return domain.Release{}, false, nil
	}
	return r.findReleaseBySlug(ctx, strings.TrimSuffix(filename, ".tar.gz"))
}

func releaseV3FileURI(req *http.Request, release domain.Release) string {
	return httputil.ExternalBaseURL(req, "") + "/v3/files/" + releaseV3FileName(release)
}

func releaseV3FileName(release domain.Release) string {
	return releaseSlug(release.Owner, release.Name, release.Version) + ".tar.gz"
}

func releaseSlug(owner, name, version string) string {
	return owner + "-" + name + "-" + version
}

func moduleSlug(owner, name string) string {
	return owner + "-" + name
}
