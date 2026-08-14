package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func (r *Router) manageAccessTokenPage(w http.ResponseWriter, req *http.Request) {
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
	if r.tokenHasher == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("access token hashing is not configured"))
		return
	}
	unlock, err := r.modules.LockAccessConfig(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer releaseAccessConfigLock(unlock)

	team := strings.TrimSpace(req.FormValue("team"))
	if team == "" || (!principal.CanAdmin && !canManageAccessTeam(principal, team)) {
		writeError(w, http.StatusForbidden, errors.New("team admin access required"))
		return
	}
	next := manageReturnPath(req, "/manage/teams/"+team+"/tokens")
	configs, err := r.modules.LoadTeamConfigs(req.Context())
	if err != nil {
		redirectManageResult(w, req, next, "error", err.Error())
		return
	}
	cfg := findAccessConfig(configs, team)
	if cfg == nil || isGlobalAdminConfig(*cfg) {
		writeError(w, http.StatusNotFound, errors.New("team not found"))
		return
	}

	switch req.FormValue("action") {
	case "create":
		r.createAccessToken(w, req, principal, configs, cfg, strings.TrimSpace(req.FormValue("role")), nil, next)
	case "rotate":
		kind, existing := accessTokenRecordByID(cfg, req.FormValue("token_id"))
		if existing == nil || existing.RevokedAt != nil {
			writeError(w, http.StatusNotFound, errors.New("access token not found"))
			return
		}
		r.createAccessToken(w, req, principal, configs, cfg, kind, existing, next)
	case "revoke":
		_, record := accessTokenRecordByID(cfg, req.FormValue("token_id"))
		if record == nil || record.RevokedAt != nil {
			writeError(w, http.StatusNotFound, errors.New("access token not found"))
			return
		}
		revokedAt := time.Now().UTC()
		record.RevokedAt = &revokedAt
		if err := r.saveAccessConfigs(req.Context(), configs); err != nil {
			r.audit(req, principal, "revoke_access_token", "failure", auditReason(err), "team", team, "token_id", record.ID)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		r.audit(req, principal, "revoke_access_token", "success", "none", "team", team, "token_id", record.ID)
		redirectManageResult(w, req, next, "message", "access token revoked")
	default:
		writeError(w, http.StatusBadRequest, errors.New("unknown token action"))
	}
}

func (r *Router) createAccessToken(w http.ResponseWriter, req *http.Request, principal auth.Principal, configs []auth.TeamConfig, cfg *auth.TeamConfig, kind string, rotated *auth.AccessTokenRecord, next string) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "read" && kind != "publish" {
		writeError(w, http.StatusBadRequest, errors.New("access token role must be read or publish"))
		return
	}
	name := strings.TrimSpace(req.FormValue("name"))
	if rotated != nil {
		name = rotated.Description
	} else if name == "" {
		writeError(w, http.StatusBadRequest, errors.New("token name is required"))
		return
	}
	if len(name) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("token name must not exceed 200 characters"))
		return
	}
	if rotated == nil && activeTokenNameExists(cfg, kind, name, time.Now()) {
		writeError(w, http.StatusConflict, errors.New("an active token with this name and role already exists"))
		return
	}

	raw, record, err := auth.GenerateAccessToken(kind, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	record.Description = name
	if rotated != nil {
		record.ExpiresAt = rotated.ExpiresAt
	}
	if rotated == nil {
		expiresAt, err := tokenExpiry(req.FormValue("expires_days"), time.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		record.ExpiresAt = expiresAt
	}
	record, err = r.tokenHasher.Record(raw, record)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rotated != nil {
		revokedAt := time.Now().UTC()
		rotated.RevokedAt = &revokedAt
	}
	if kind == "read" {
		cfg.ReadTokenRecords = append(cfg.ReadTokenRecords, record)
	} else {
		cfg.PublishTokenRecords = append(cfg.PublishTokenRecords, record)
	}
	if err := r.saveAccessConfigs(req.Context(), configs); err != nil {
		r.audit(req, principal, "create_access_token", "failure", auditReason(err), "team", cfg.Team, "token_role", kind)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	action := "create_access_token"
	if rotated != nil {
		action = "rotate_access_token"
	}
	r.audit(req, principal, action, "success", "none", "team", cfg.Team, "token_role", kind, "token_id", record.ID)
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageTokenCreatedTemplate, manageTokenCreatedData{
		Navigation: r.manageNavigation(req, principal, csrfToken, "token", cfg.Team),
		Token:      raw, Name: name, Prefix: record.Prefix, Team: cfg.Team, Kind: kind, Next: next, CSRFToken: csrfToken,
	})
}

func activeTokenNameExists(cfg *auth.TeamConfig, kind, name string, now time.Time) bool {
	var records []auth.AccessTokenRecord
	switch kind {
	case "read":
		records = cfg.ReadTokenRecords
	case "publish":
		records = cfg.PublishTokenRecords
	default:
		return false
	}
	for _, record := range records {
		if !strings.EqualFold(strings.TrimSpace(record.Description), name) || record.RevokedAt != nil {
			continue
		}
		if record.ExpiresAt == nil || now.Before(*record.ExpiresAt) {
			return true
		}
	}
	return false
}

func tokenExpiry(raw string, now time.Time) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return nil, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > 3650 {
		return nil, errors.New("token expiry must be between 1 and 3650 days")
	}
	expiresAt := now.Add(time.Duration(days) * 24 * time.Hour).UTC()
	return &expiresAt, nil
}

func accessTokenRecordByID(cfg *auth.TeamConfig, tokenID string) (string, *auth.AccessTokenRecord) {
	tokenID = strings.TrimSpace(tokenID)
	for i := range cfg.ReadTokenRecords {
		if cfg.ReadTokenRecords[i].ID == tokenID {
			return "read", &cfg.ReadTokenRecords[i]
		}
	}
	for i := range cfg.PublishTokenRecords {
		if cfg.PublishTokenRecords[i].ID == tokenID {
			return "publish", &cfg.PublishTokenRecords[i]
		}
	}
	return "", nil
}
