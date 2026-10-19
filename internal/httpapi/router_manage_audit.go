package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (r *Router) manageAuditPage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	principal, ok := r.requireManage(w, req)
	if !ok {
		return
	}
	if !principal.CanAdmin {
		writeError(w, http.StatusForbidden, errors.New("global admin access required"))
		return
	}
	r.renderManageAudit(w, req, principal, nil, "/manage/admin/audit", "audit", "", "Audit log")
}

func (r *Router) renderManageAudit(w http.ResponseWriter, req *http.Request, principal auth.Principal, scopes []string, basePath, navigationPage, navigationTeam, title string) {
	query, page, pageSize, err := requestedManageList(req, "audit-list")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	events, total, err := r.modules.ListAuditEvents(req.Context(), scopes, query, pageSize, (page-1)*pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	canonicalPage, changed := normalizedListPage(page, pageSize, total)
	canonicalURL := manageListPageURL(basePath, req.URL.Query(), "audit-list", canonicalPage)
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return
	}
	pagination, err := manageListPagination(basePath, req.URL.Query(), "audit-list", canonicalPage, pageSize, total)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	csrfToken, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageAuditTemplate, manageAuditData{
		Navigation: r.manageNavigation(req, principal, csrfToken, navigationPage, navigationTeam),
		Title:      title,
		Events:     manageAuditRows(events),
		Query:      query,
		Filter: newListFilter(
			"audit-filter", "audit-query", basePath, "audit-list",
			"Filter audit events", "q", "Filter by actor, action, target, result, or comment", query, basePath,
		),
		Pagination: pagination,
	})
}

func manageAuditRows(events []store.AuditEvent) []manageAuditRow {
	rows := make([]manageAuditRow, 0, len(events))
	for _, event := range events {
		occurredAt := event.OccurredAt.UTC()
		rows = append(rows, manageAuditRow{
			OccurredAt:    occurredAt.Format(time.RFC3339),
			OccurredDate:  occurredAt.Format("2006-01-02"),
			OccurredTime:  occurredAt.Format("15:04:05 UTC"),
			Actor:         event.Actor,
			AuthMethod:    event.AuthMethod,
			Action:        strings.ReplaceAll(event.Action, "_", " "),
			Result:        event.Result,
			Reason:        event.Reason,
			Target:        auditTarget(event.Attributes),
			Details:       event.Attributes["changes"],
			Justification: event.Attributes["justification"],
			TargetTokenID: event.Attributes["token_id"],
			ActorTokenID:  event.Attributes["actor_token_id"],
			OIDCSubject:   event.Attributes["oidc_subject"],
			RequestID:     event.RequestID,
			SourceIP:      event.SourceIP,
		})
	}
	return rows
}

func auditTarget(attributes map[string]string) string {
	if module := attributes["module"]; module != "" {
		if release := attributes["release"]; release != "" {
			return module + "@" + release
		}
		return module
	}
	if tokenID := attributes["token_id"]; tokenID != "" {
		tokenReference := attributes["token_prefix"]
		if tokenReference == "" {
			tokenReference = tokenID
		}
		target := "token:" + tokenReference
		if tokenName := attributes["token_name"]; tokenName != "" {
			target = tokenName + " (" + attributes["token_role"] + ", " + tokenReference + ")"
		}
		return target
	}
	if team := attributes["team"]; team != "" {
		if space := attributes["space"]; space != "" {
			return "team:" + team + " / space:" + space
		}
		if newTeam := attributes["new_team"]; newTeam != "" {
			return "team:" + team + " -> " + newTeam
		}
		return "team:" + team
	}
	if space := attributes["space"]; space != "" {
		return "space:" + space
	}
	return "-"
}
