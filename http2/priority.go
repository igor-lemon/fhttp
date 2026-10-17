package http2

import (
	"context"

	http "github.com/igor-lemon/fhttp"
)

type headerPriorityKey struct{}

// WithHeaderPriority is ctx with the priority the HEADERS frame of a request
// made with it carries, in place of the Transport's HeaderPriority: a browser
// gives a page and a script's request different ones on one connection.
func WithHeaderPriority(ctx context.Context, priority PriorityParam) context.Context {
	return context.WithValue(ctx, headerPriorityKey{}, priority)
}

// headerPriority is the priority on the HEADERS frame of req: its context's,
// else the Transport's HeaderPriority, else exclusive on no stream, weight
// 256.
func (cc *ClientConn) headerPriority(req *http.Request) PriorityParam {
	if priority, ok := req.Context().Value(headerPriorityKey{}).(PriorityParam); ok {
		return priority
	}
	if cc.t.HeaderPriority != nil {
		return *cc.t.HeaderPriority
	}
	return PriorityParam{Exclusive: true, Weight: 255}
}
