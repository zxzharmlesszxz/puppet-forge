package metrics

import (
	"errors"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

var controlOperations = []string{"approve", "deprecate", "restore", "block", "unblock", "clear_cache", "delete", "force_delete", "review", "compare", "review_download", "other"}

var authorizationScopes = []string{"read", "publish", "delete", "manage", "token_active", "rate_limit_publish", "rate_limit_manage-login", "rate_limit_manage-publish", "rate_limit_auth-login", "rate_limit_auth-callback", "other"}

var securityActions = []string{"token_login", "oidc_login", "oidc_logout", "manage_logout", "publish_module", "replace_module_release", "delete_module", "delete_release", "force_delete_module", "force_delete_release", "clear_upstream_cache", "moderate_resource", "review_release", "review_release_archive", "import_upstream_module", "create_team", "update_team", "rename_team", "delete_team", "create_access_token", "rotate_access_token", "revoke_access_token", "change_global_access", "change_access_config", "other"}

var controlOperationsTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "puppet_forge_control_operations_total",
	Help: "Completed administrative operation attempts, excluding transport validation before the service call.",
}, []string{"operation", "kind", "result"}))

var authorizationChecksTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "puppet_forge_authorization_checks_total",
	Help: "Authorization gate decisions; one request can pass more than one gate.",
}, []string{"scope", "result"}))

var auditWritesTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "puppet_forge_audit_writes_total",
	Help: "Audit persistence attempts, including the enclosing mutation commit when transactional.",
}, []string{"result"}))

var accessConfigRefreshTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "puppet_forge_access_config_refresh_total",
	Help: "Actual access configuration refresh attempts; cached and coalesced reads are excluded.",
}, []string{"result"}))

var securityEventsTotal = RegisterCounterVec(prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "puppet_forge_security_events_total",
	Help: "Security audit events by bounded action category, independently of persistence success.",
}, []string{"action", "result"}))

func init() {
	for _, operation := range controlOperations {
		for _, kind := range []string{"module", "release"} {
			for _, result := range []string{"success", "rejected", "error"} {
				controlOperationsTotal.WithLabelValues(operation, kind, result).Add(0)
			}
		}
	}
	for _, scope := range authorizationScopes {
		for _, result := range []string{"success", "rejected", "error"} {
			authorizationChecksTotal.WithLabelValues(scope, result).Add(0)
		}
	}
	for _, result := range []string{"success", "error"} {
		auditWritesTotal.WithLabelValues(result).Add(0)
		accessConfigRefreshTotal.WithLabelValues(result).Add(0)
	}
	for _, action := range securityActions {
		for _, result := range []string{"success", "failure"} {
			securityEventsTotal.WithLabelValues(action, result).Add(0)
		}
	}
}

func boundedMetricLabel(value string, allowed ...string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return "other"
}

// ObserveOutcome records a deferred result and rethrows panics after recording
// an error, rather than incorrectly reporting the zero-value error as success.
func ObserveOutcome(err *error, observer func(error)) {
	if recovered := recover(); recovered != nil {
		observer(errors.New("operation panicked"))
		panic(recovered)
	}
	observer(*err)
}

func ObserveControlOperation(operation, kind, result string) {
	controlOperationsTotal.WithLabelValues(boundedMetricLabel(operation, controlOperations...), boundedMetricLabel(kind, "module", "release"), boundedMetricLabel(result, "success", "rejected", "error")).Inc()
}

func ObserveAuthorization(scope, result string) {
	authorizationChecksTotal.WithLabelValues(boundedMetricLabel(scope, authorizationScopes...), boundedMetricLabel(result, "success", "rejected", "error")).Inc()
}

func ObserveAuditWrite(err error) {
	auditWritesTotal.WithLabelValues(resultLabel(err)).Inc()
}

func ObserveAccessConfigRefresh(err error) {
	accessConfigRefreshTotal.WithLabelValues(resultLabel(err)).Inc()
}

func ObserveSecurityEvent(action, result string) {
	category := boundedMetricLabel(action, securityActions...)
	securityEventsTotal.WithLabelValues(category, boundedMetricLabel(result, "success", "failure")).Inc()
}
