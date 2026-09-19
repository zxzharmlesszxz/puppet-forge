package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

func (r *Router) manageLegacyUsagePage(w http.ResponseWriter, req *http.Request) {
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
	r.renderManageLegacyUsage(w, req, principal, "", "/manage/admin/legacy", "legacy-usage", "Legacy module usage")
}

func (r *Router) renderManageLegacyUsage(w http.ResponseWriter, req *http.Request, principal auth.Principal, consumerTeam, basePath, navigationPage, title string) {
	query, page, pageSize, err := requestedManageList(req, "legacy-usage-list")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	consumers, total, err := r.modules.ListLegacyReleaseConsumers(req.Context(), consumerTeam, query, pageSize, (page-1)*pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	canonicalPage, changed := normalizedListPage(page, pageSize, total)
	canonicalURL := manageListPageURL(basePath, req.URL.Query(), "legacy-usage-list", canonicalPage)
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return
	}
	pagination, err := manageListPagination(basePath, req.URL.Query(), "legacy-usage-list", canonicalPage, pageSize, total)
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
	executeHTMLTemplate(w, manageLegacyUsageTemplate, manageLegacyUsageData{
		Navigation: r.manageNavigation(req, principal, csrfToken, navigationPage, consumerTeam),
		Title:      title,
		Consumers:  manageLegacyConsumerRows(consumers),
		Query:      query,
		Filter: newListFilter(
			"legacy-usage-filter", "legacy-usage-query", basePath, "legacy-usage-list",
			"Filter legacy usage", "q", "Filter by team, token, role, module, or version", query, basePath,
		),
		Pagination: pagination,
	})
}

func manageLegacyConsumerRows(consumers []store.ReleaseConsumer) []manageLegacyConsumerRow {
	rows := make([]manageLegacyConsumerRow, 0, len(consumers))
	for _, consumer := range consumers {
		modulePath := "/modules/" + url.PathEscape(consumer.Owner) + "/" + url.PathEscape(consumer.Name)
		rows = append(rows, manageLegacyConsumerRow{
			ConsumerTeam: consumer.ConsumerTeam,
			ConsumerName: consumer.ConsumerName,
			ConsumerRole: consumer.ConsumerRole,
			Module:       consumer.Owner + "/" + consumer.Name,
			ModuleURL:    modulePath + "?version=" + url.QueryEscape(consumer.Version),
			Version:      consumer.Version,
			Latest:       consumer.LatestVersion,
			FirstSeen:    consumer.FirstSeenAt.UTC().Format(time.RFC3339),
			LastSeen:     consumer.LastSeenAt.UTC().Format(time.RFC3339),
			Observations: consumer.Observations,
		})
	}
	return rows
}
