// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import "slices"

// The stream a request's HEADERS depend on, as Chrome's network stack chooses
// it (net/spdy/http2_priority_dependencies.cc, measured on Chrome 155 by
// tls-forge) under "h2.priority_dependency": "nearest-weight". The streams of
// a connection that are open and whose HEADERS said a priority are kept in
// lists, one for each weight, in the order they were opened. A new stream
// depends on the last of the list of the lowest weight not below its own, or
// on none, and joins its own weight's list; it leaves it as it leaves the
// connection (forgetStreamLocked): its answer ended, or it was reset. A
// weight is Chrome's priority as the wire says it, so a heavier one is a
// higher priority. "root", or no policy, leaves the dependency the priority
// gives, none.

// dependOnLocked is p with the stream that stream id, about to be opened,
// depends on, and puts id in its weight's list. A priority that says nothing
// (no PRIORITY flag) is left as it is. Requires cc.mu.
func (cc *ClientConn) dependOnLocked(id uint32, p PriorityParam) PriorityParam {
	if !cc.nearestWeight || p.IsZero() {
		return p
	}
	p.StreamDep = 0
	for w := int(p.Weight); w <= 255; w++ {
		if open := cc.byWeight[uint8(w)]; len(open) > 0 {
			p.StreamDep = open[len(open)-1]
			break
		}
	}
	if cc.byWeight == nil {
		cc.byWeight = map[uint8][]uint32{}
		cc.weightOf = map[uint32]uint8{}
	}
	cc.byWeight[p.Weight] = append(cc.byWeight[p.Weight], id)
	cc.weightOf[id] = p.Weight
	return p
}

// undependLocked takes stream id out of its weight's list. Requires cc.mu.
func (cc *ClientConn) undependLocked(id uint32) {
	w, ok := cc.weightOf[id]
	if !ok {
		return
	}
	delete(cc.weightOf, id)
	open := cc.byWeight[w]
	i := slices.Index(open, id)
	cc.byWeight[w] = slices.Delete(open, i, i+1)
}
