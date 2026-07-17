package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func (r *Router) indexPage(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	modules, err := r.modules.ListModules(req.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexPageTemplate.Execute(w, indexPageData{
		Modules:   modules,
		AuthLink:  r.indexAuthLink(),
		AuthLabel: r.indexAuthLabel(),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
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
