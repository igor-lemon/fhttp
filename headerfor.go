package http

// The protocols a request is written in, as Request.HeaderFor is told them:
// their ALPN identifiers.
const (
	ProtocolHTTP1 = "http/1.1"
	ProtocolHTTP2 = "h2"
)

// WrittenHeader is the header r is written with over protocol: the one its
// HeaderFor gives, else Header.
func (r *Request) WrittenHeader(protocol string) Header {
	if r.HeaderFor != nil {
		if header := r.HeaderFor(protocol, r.Header); header != nil {
			return header
		}
	}
	return r.Header
}
