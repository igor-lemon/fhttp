package http

import (
	"context"
	"net/url"
	"strings"
)

type originCredentialsKey struct{}

// WithCredentialsKeptToOrigin is ctx under which a Client copies a request's
// credentials, the fields sensitiveRedirectHeader names, onto the hops of its
// redirects only while they stay at the request's origin: its scheme, host and
// port. Without it they go on to a subdomain of its host too, as net/http has
// them go. Either way no hop after one they were not copied to gets them.
func WithCredentialsKeptToOrigin(ctx context.Context) context.Context {
	return context.WithValue(ctx, originCredentialsKey{}, true)
}

// credentialsGoTo reports whether the credentials ireq was given may be copied
// onto a hop to dest.
func credentialsGoTo(ireq *Request, dest *url.URL) bool {
	if kept, _ := ireq.Context().Value(originCredentialsKey{}).(bool); kept {
		return strings.EqualFold(ireq.URL.Scheme, dest.Scheme) && strings.EqualFold(canonicalAddr(ireq.URL), canonicalAddr(dest))
	}
	return shouldCopySensitiveHeadersOnRedirect(ireq.URL, dest)
}
