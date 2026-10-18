package http

import "context"

type responseCookiesKey struct{}

// WithResponseCookies is ctx with keep, which a Client asks, of each answer
// to a request made with ctx, whether its Jar is given the cookies the answer
// sets: none when keep says no. Without one, every answer's are. Chrome's
// network stack hands no answer to what keeps cookies when it sends the
// request again for it: a 421 (Misdirected Request) sets nothing that is
// kept, nor does it reach the page.
func WithResponseCookies(ctx context.Context, keep func(*Response) bool) context.Context {
	return context.WithValue(ctx, responseCookiesKey{}, keep)
}

// keepsCookies reports whether the cookies resp, the answer to req, sets go
// to the Jar.
func keepsCookies(req *Request, resp *Response) bool {
	keep, _ := req.Context().Value(responseCookiesKey{}).(func(*Response) bool)
	return keep == nil || keep(resp)
}
