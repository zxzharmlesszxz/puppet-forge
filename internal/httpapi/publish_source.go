package httpapi

import (
	"errors"
	"net/http"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func (r *Router) requirePublishSource(w http.ResponseWriter, req *http.Request, principal auth.Principal) bool {
	if len(r.publishAllowedCIDRs) == 0 {
		return true
	}
	address, ok := parseRequestAddress(clientAddress(req, r.trustedProxyCIDRs))
	if ok && addressInPrefixes(address, r.publishAllowedCIDRs) {
		return true
	}
	r.audit(req, principal, "publish_module", "failure", "publish_source_forbidden")
	writeError(w, http.StatusForbidden, errors.New("client IP is not allowed to publish"))
	return false
}
