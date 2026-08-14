package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
	"github.com/zxzharmlesszxz/puppet-forge/internal/metrics"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Warn("encode json response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	code := errorCode(status, err)
	message := safeErrorMessage(status, err)
	requestID := w.Header().Get(requestIDHeader)
	if status >= http.StatusInternalServerError {
		slog.Error("http request failed", "request_id", requestID, "status", status, "error_code", code, "err", err)
	}
	writeJSON(w, status, map[string]string{
		"error":      message,
		"code":       code,
		"request_id": requestID,
	})
}

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, service.ErrValidation):
		status = http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound), errors.Is(err, storage.ErrObjectNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, service.ErrProtectedDelete):
		status = http.StatusConflict
	case errors.Is(err, service.ErrUpstreamHydration):
		status = http.StatusBadGateway
	}
	writeError(w, status, err)
}

func errorCode(status int, err error) string {
	switch {
	case errors.Is(err, service.ErrProtectedDelete):
		return "protected_resource"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	}
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusBadGateway:
		return "upstream_unavailable"
	case http.StatusServiceUnavailable:
		return "storage_unavailable"
	case http.StatusInternalServerError:
		return "internal_error"
	default:
		return "validation_error"
	}
}

func safeErrorMessage(status int, err error) string {
	if status < http.StatusInternalServerError {
		return err.Error()
	}
	switch status {
	case http.StatusBadGateway:
		return "upstream service is unavailable"
	case http.StatusServiceUnavailable:
		return "service is unavailable"
	default:
		return "internal server error"
	}
}

func (r *Router) writeReleaseArchive(w http.ResponseWriter, req *http.Request, release domain.Release) {
	release, err := r.modules.EnsureReleaseChecksums(req.Context(), release)
	if err != nil {
		clearArtifactResponseHeaders(w.Header())
		writeServiceError(w, err)
		return
	}

	attrs, err := r.modules.StatReleaseArchive(req.Context(), release)
	if err != nil {
		clearArtifactResponseHeaders(w.Header())
		writeServiceError(w, err)
		return
	}

	etag := releaseETag(release)
	response, err := httputil.PrepareByteRangeResponse(w.Header(), req.Header, attrs.Size, etag, attrs.ContentType)
	if err != nil {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, err)
		return
	}
	if response.NotModified {
		w.WriteHeader(response.Status)
		return
	}
	if req.Method == http.MethodHead {
		w.WriteHeader(response.Status)
		return
	}

	var object storage.ObjectReader
	if response.Partial {
		object, err = r.modules.OpenReleaseArchiveRange(req.Context(), release, response.Range.Start, response.Range.Length)
	} else {
		object, err = r.modules.OpenReleaseArchive(req.Context(), release.Owner, release.Name, release.Version)
	}
	if err != nil {
		clearArtifactResponseHeaders(w.Header())
		writeServiceError(w, err)
		return
	}
	defer func() {
		if err := object.Body.Close(); err != nil {
			slog.Warn("close object response", "err", err)
		}
	}()
	w.WriteHeader(response.Status)
	written, err := io.Copy(w, object.Body)
	result := "success"
	if err != nil {
		result = "error"
		if req.Context().Err() != nil {
			result = "client_cancel"
		}
		slog.Warn("stream object response", "err", err)
	}
	source := "local"
	if release.Source == "upstream" {
		source = "upstream_cache"
	}
	metrics.ObserveArtifactStream(source, result, written)
}

func clearArtifactResponseHeaders(header http.Header) {
	httputil.ClearByteRangeResponseHeaders(header)
}

func releaseETag(release domain.Release) string {
	if strings.TrimSpace(release.SHA256) == "" {
		return ""
	}
	return `"` + strings.TrimSpace(release.SHA256) + `"`
}
