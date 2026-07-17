package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/zxzharmlesszxz/puppet-forge/internal/storage"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Warn("encode json response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{
		"error": err.Error(),
	})
}

func writeObject(w http.ResponseWriter, req *http.Request, object storage.Object) {
	if object.ContentType != "" {
		w.Header().Set("Content-Type", object.ContentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(object.Body)))
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(object.Body)
}
