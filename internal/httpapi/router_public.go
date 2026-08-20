package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const publicModulePageSize = 20

func (r *Router) indexPage(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	owner := strings.TrimSpace(req.URL.Query().Get("owner"))
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	pageSize := requestedPageSize(req, "/", "per_page", "module-list", publicModulePageSize)
	page, err := requestedManagePageWithSize(req, pageSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var owners []string
	if owner != "" {
		owners = []string{owner}
	}
	modules, total, err := r.modules.ListModulesPageFiltered(req.Context(), owners, query, pageSize, (page-1)*pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	canonicalPage, changed := normalizedListPage(page, pageSize, total)
	canonicalURL := managePageURLWithValues("/", req.URL.Query(), canonicalPage, "module-list")
	if handleCanonicalListPage(w, req, changed, canonicalURL) {
		return
	}
	if changed {
		page = canonicalPage
		modules, total, err = r.modules.ListModulesPageFiltered(req.Context(), owners, query, pageSize, (page-1)*pageSize)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	pagination := managePaginationForRequest("/", req.URL.Query(), "module-list", "module-list", page, pageSize, total)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	navigation := r.publicNavigation(req, r.indexAuthLink(), r.indexAuthLabel(), publicBreadcrumb{Label: "Modules", Current: owner == ""})
	if owner != "" {
		navigation.Breadcrumbs[0].URL = "/"
		navigation.Breadcrumbs = append(navigation.Breadcrumbs, publicBreadcrumb{Label: owner, Current: true})
	}
	filterParams := []queryParameter(nil)
	filterClearURL := "/"
	filterPlaceholder := "Filter by owner or module name"
	if owner != "" {
		filterParams = []queryParameter{{Name: "owner", Value: owner}}
		filterPlaceholder = "Filter " + owner + " modules"
		filterClearURL = "/?owner=" + url.QueryEscape(owner)
	}
	executeHTMLTemplate(w, indexPageTemplate, indexPageData{
		Navigation: navigation,
		Modules:    modules,
		Owner:      owner,
		Query:      query,
		Filter: newListFilter(
			"module-filter-form", "module-filter", "/", "module-list", "Filter modules", "q",
			filterPlaceholder, query, filterClearURL, filterParams...,
		),
		Pagination: pagination,
	})
}

func (r *Router) indexAuthLink() string {
	return "/manage"
}

func (r *Router) indexAuthLabel() string {
	return "Manage"
}

func (r *Router) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (r *Router) readyz(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()

	if err := r.modules.Ready(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
