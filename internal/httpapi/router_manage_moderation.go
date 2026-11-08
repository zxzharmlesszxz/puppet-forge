package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
)

type manageModerationData struct {
	Navigation  manageNavigationData
	Resource    string
	Status      domain.ResourceStatus
	SHA256      string
	CSRFToken   string
	Next        string
	Version     string
	DownloadURL string
	Actions     []string
}

func manageModerationURL(owner, name, version string) string {
	return "/manage/modules/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/moderate?version=" + url.QueryEscape(version)
}

func (r *Router) manageModeration(w http.ResponseWriter, req *http.Request, principal auth.Principal, owner, name string) {
	if !canDeleteInSpace(principal, owner) {
		writeError(w, http.StatusForbidden, errors.New("team administrator access required"))
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if req.Method == http.MethodPost && (!r.requirePublishSource(w, req, principal) || !r.requireManageCSRF(w, req)) {
		return
	}
	version := req.URL.Query().Get("version")
	module, err := r.modules.GetModule(req.Context(), owner, name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data := manageModerationData{Resource: owner + "/" + name, Status: module.Status, Version: version, Next: "/manage/modules"}
	allowed, err := r.modules.CanDeleteModule(req.Context(), principal, owner, name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !allowed {
		writeServiceError(w, service.ErrModerationForbidden)
		return
	}
	if version != "" {
		release, err := r.modules.GetStoredRelease(req.Context(), owner, name, version)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		data.Resource += "@" + version
		data.SHA256 = release.SHA256
		data.Status = release.Status
		if release.Status.State == domain.StatePending {
			data.DownloadURL = manageModerationURL(owner, name, version) + "&download=true"
		} else if release.Status.State.Readable() && module.Status.State.Readable() {
			data.DownloadURL = manageModerationURL(owner, name, version) + "&download=true"
		}
	}
	if req.Method == http.MethodGet && req.URL.Query().Get("download") == "true" {
		if data.Status.State != domain.StatePending {
			release, err := r.modules.GetRelease(req.Context(), owner, name, version)
			if err != nil {
				writeServiceError(w, err)
				return
			}
			r.writeReleaseArchive(w, req, release)
			return
		}
		object, err := r.modules.ReviewArchive(req.Context(), owner, name, version)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		defer func() {
			if err := object.Body.Close(); err != nil {
				slog.Warn("close review archive", "err", err)
			}
		}()
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="review.tar.gz"`)
		r.audit(req, principal, "review_release_archive", "success", "none", "space", owner, "module", owner+"/"+name, "release", version)
		if _, err := io.Copy(w, object.Body); err != nil {
			slog.Warn("stream review archive", "err", err)
		}
		return
	}
	if req.Method == http.MethodPost {
		action, reason := req.FormValue("action"), req.FormValue("reason")
		r.prepareMutationAudit(req, principal, "moderate_resource", "justification", reason, "moderation_action", action, "space", owner, "module", owner+"/"+name, "release", version)
		err := r.modules.Moderate(req.Context(), principal, owner, name, version, action, reason, req.FormValue("replacement"), req.FormValue("sha256"))
		if err != nil {
			r.audit(req, principal, "moderate_resource", "failure", auditReason(err), "space", owner, "module", owner+"/"+name, "release", version)
			writeServiceError(w, err)
			return
		}
		redirectManageResult(w, req, data.Next, "message", "moderation decision saved")
		return
	}
	switch data.Status.State {
	case domain.StatePending:
		if version != "" {
			data.Actions = []string{"approve", "block"}
		} else {
			data.Actions = []string{"block"}
		}
	case domain.StateBlocked:
		data.Actions = []string{"unblock"}
	case domain.StateDeprecated:
		data.Actions = []string{"restore", "block"}
	default:
		data.Actions = []string{"deprecate", "block"}
	}
	csrf, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data.CSRFToken = csrf
	data.Navigation = r.manageNavigation(req, principal, csrf, "modules", "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageModerationTemplate, data)
}
