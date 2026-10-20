package webauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/zxzharmlesszxz/puppet-forge/internal/observability"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
	"golang.org/x/oauth2"
)

func logOIDCStoreFailure(ctx context.Context, operation string, err error) {
	if (operation == "read_session" || operation == "consume_state") && errors.Is(err, store.ErrNotFound) {
		return
	}
	slog.Error("OIDC storage operation failed", "request_id", observability.RequestID(ctx), "operation", operation, "err", err)
}

func logOIDCProtocolFailure(req *http.Request, operation string, err error) {
	attrs := []any{"request_id", observability.RequestID(req.Context()), "operation", operation}
	if err != nil {
		// Protocol errors can contain tokens, response bodies or credential-bearing URLs.
		attrs = append(attrs, "error_type", fmt.Sprintf("%T", err))
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		attrs = append(attrs, "reason", "deadline_exceeded")
	case errors.Is(err, context.Canceled):
		attrs = append(attrs, "reason", "context_canceled")
	}
	if responseError, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		if responseError.Response != nil {
			attrs = append(attrs, "upstream_status", responseError.Response.StatusCode)
		}
		switch responseError.ErrorCode {
		case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type", "invalid_scope", "server_error", "temporarily_unavailable":
			attrs = append(attrs, "provider_error", responseError.ErrorCode)
		default:
			attrs = append(attrs, "provider_error", "unrecognized_error")
		}
	}
	slog.Warn("OIDC protocol operation failed", attrs...)
}
