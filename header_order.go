// Copyright 2022-2026 Sauce Labs Inc., all rights reserved.

package forwarder

import (
	"context"

	"github.com/saucelabs/forwarder/internal/martian"
)

// WithHeaderOrder returns ctx carrying order as the client's header order. The
// proxy sets this for every request it parses; it is exported so that code
// downstream of the proxy can be tested without standing one up.
func WithHeaderOrder(ctx context.Context, order []string) context.Context {
	return martian.WithHeaderOrder(ctx, order)
}

// HeaderOrderFromContext returns the order in which the client sent the headers
// of the request carrying ctx, lowercased, or nil when the order could not be
// recovered. Callers that re-originate the request can use it to reproduce the
// client's header order, which net/http's header map does not preserve and which
// origins fingerprint.
//
// The names are exactly what the client sent: they include HTTP/1.1-only headers
// such as Host and Connection, and exclude headers added by request modifiers.
func HeaderOrderFromContext(ctx context.Context) []string {
	return martian.HeaderOrderFromContext(ctx)
}
