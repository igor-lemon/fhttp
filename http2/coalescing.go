package http2

import (
	"context"

	http "github.com/igor-lemon/fhttp"
)

type coalescingKey struct{}

// WithCoalescing is ctx saying whether a request made with it may go out on
// a connection the default pool shares with its host:port though it was
// dialed for another's (Transport.Coalesce). One that may not goes out only
// on a connection dialed for its own host:port, one dialed for it when the
// pool holds none that can take it, and Coalesce is not asked. A request
// whose context says nothing of it may. Chrome sends a request a server
// answered 421 (Misdirected Request) again with IP-based pooling off, so on
// a connection of its host's own (HttpNetworkTransaction).
func WithCoalescing(ctx context.Context, allowed bool) context.Context {
	return context.WithValue(ctx, coalescingKey{}, allowed)
}

// mayCoalesce reports whether req may go out on a connection shared with its
// host:port: unless its context says it may not.
func mayCoalesce(req *http.Request) bool {
	allowed, said := req.Context().Value(coalescingKey{}).(bool)
	return allowed || !said
}
