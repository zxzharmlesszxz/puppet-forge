package httpapi

import (
	"context"
	"errors"
	"net/http"
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
	r.forgeProxy.ServeHTTP(w, req)
}

func (r *Router) serveLocalV3Response(w http.ResponseWriter, req *http.Request) bool {
	trimmed := strings.Trim(strings.TrimPrefix(req.URL.Path, "/v3/"), "/")
	parts := strings.Split(trimmed, "/")
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
		return false
	}

	r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
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
