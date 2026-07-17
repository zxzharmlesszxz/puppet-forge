package httpapi

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
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

	if module.LatestVersion != "" {
		r.markReleaseUsed(req.Context(), module.Owner, module.Name, module.LatestVersion)
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

	response := map[string]any{
		"slug":  slug,
		"owner": module.Owner,
		"name":  module.Name,
		"current_release": releaseRef{
			Slug:    releaseSlug(module.Owner, module.Name, module.LatestVersion),
			Version: module.LatestVersion,
		},
		"releases": releases,
	}
	writeJSON(w, http.StatusOK, response)
	return true
}

func (r *Router) serveLocalV3Release(w http.ResponseWriter, req *http.Request, slug string) bool {
	release, ok, err := r.findReleaseBySlug(req.Context(), slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return true
	}
	if !ok {
		return false
	}
	r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
	fileSize := release.SizeBytes
	fileMD5 := ""
	fileSHA256 := release.SHA256

	archive, err := r.modules.ReadReleaseArchive(req.Context(), release.Owner, release.Name, release.Version)
	if err == nil {
		fileSize = int64(len(archive.Body))
		md5Sum := md5.Sum(archive.Body)
		fileMD5 = hex.EncodeToString(md5Sum[:])
		shaSum := sha256.Sum256(archive.Body)
		fileSHA256 = hex.EncodeToString(shaSum[:])
	} else if errors.Is(err, store.ErrNotFound) {
		if release.Source != "upstream" {
			writeError(w, http.StatusNotFound, err)
			return true
		}
	} else {
		writeError(w, http.StatusInternalServerError, err)
		return true
	}

	response := map[string]any{
		"slug":        releaseSlug(release.Owner, release.Name, release.Version),
		"version":     release.Version,
		"file_uri":    releaseV3FileURI(req, release),
		"file_name":   releaseV3FileName(release),
		"file_size":   fileSize,
		"readme":      release.Readme,
		"description": release.Description,
		"metadata":    release.Metadata,
	}
	if fileMD5 != "" {
		response["file_md5"] = fileMD5
	}
	if fileSHA256 != "" {
		response["file_sha256"] = fileSHA256
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
	if release.Source == "upstream" {
		r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
		return false
	}

	object, err := r.modules.ReadReleaseArchive(req.Context(), release.Owner, release.Name, release.Version)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return true
	}

	r.markReleaseUsed(req.Context(), release.Owner, release.Name, release.Version)
	writeObject(w, req, object)
	return true
}

func (r *Router) findModuleBySlug(ctx context.Context, slug string) (domain.Module, bool, error) {
	owner, rest, ok := strings.Cut(slug, "-")
	if !ok || owner == "" || rest == "" {
		return domain.Module{}, false, nil
	}
	parts := strings.Split(rest, "-")
	for i := len(parts); i > 0; i-- {
		name := strings.Join(parts[:i], "-")
		module, err := r.modules.GetModule(ctx, owner, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return domain.Module{}, false, err
		}
		return module, true, nil
	}
	return domain.Module{}, false, nil
}

func (r *Router) findReleaseBySlug(ctx context.Context, slug string) (domain.Release, bool, error) {
	owner, rest, ok := strings.Cut(slug, "-")
	if !ok || owner == "" || rest == "" {
		return domain.Release{}, false, nil
	}
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] != '-' {
			continue
		}
		name := rest[:i]
		version := rest[i+1:]
		if name == "" || version == "" {
			continue
		}
		release, err := r.modules.GetRelease(ctx, owner, name, version)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return domain.Release{}, false, err
		}
		return release, true, nil
	}
	return domain.Release{}, false, nil
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
