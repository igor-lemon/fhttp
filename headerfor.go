package http

// The protocols a request is written in, as Request.HeaderFor is told them:
// their ALPN identifiers.
const (
	ProtocolHTTP1 = "http/1.1"
	ProtocolHTTP2 = "h2"
	ProtocolHTTP3 = "h3"
)

// WrittenHeader returns an owned copy of the header selected for protocol.
// HeaderFor receives a mutable working copy of Header. The transport does not
// modify either that argument after the callback or a map returned by it. A
// nil result selects the working copy, including the callback's changes.
func (r *Request) WrittenHeader(protocol string) Header {
	header := r.Header.Clone()
	if r.HeaderFor != nil {
		if header == nil {
			header = make(Header)
		}
		if selected := r.HeaderFor(protocol, header); selected != nil {
			return selected.Clone()
		}
		return header.Clone()
	}
	return header
}
