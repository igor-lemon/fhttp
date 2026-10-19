// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// headerFrames are the payload lengths of the frames that carried the header
// block of a request with an X-Big of n letters, as a transport whose
// MaxHeaderFramePayload is limit and whose HEADERS priority is priority (the
// transport's own for nil) wrote it to a server that said settings, after a
// first request the server's SETTINGS came before; and whether its HEADERS
// frame said a priority.
func headerFrames(t *testing.T, limit uint32, priority *PriorityParam, n int, settings ...Setting) ([]int, bool) {
	t.Helper()
	ct := newClientTester(t)
	ct.tr.MaxHeaderFramePayload = limit
	ct.tr.HeaderPriority = priority
	var frames []int
	var prioritized bool
	ct.client = func() error {
		for _, value := range []string{"", strings.Repeat("a", n)} {
			req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
			req.Header.Set("X-Big", value)
			res, err := ct.tr.RoundTrip(req)
			if err != nil {
				return err
			}
			if err := res.Body.Close(); err != nil {
				return err
			}
		}
		return nil
	}
	ct.server = func() error {
		ct.greet(settings...)
		for {
			f, err := ct.readNonSettingsFrame()
			if err != nil {
				return err
			}
			ended, stream := false, f.Header().StreamID
			switch f := f.(type) {
			case *HeadersFrame:
				if stream == 3 {
					frames, prioritized = append(frames, int(f.Header().Length)), f.HasPriority()
				}
				ended = f.HeadersEnded()
			case *ContinuationFrame:
				if stream == 3 {
					frames = append(frames, int(f.Header().Length))
				}
				ended = f.HeadersEnded()
			}
			if ended {
				var buf bytes.Buffer
				_ = hpack.NewEncoder(&buf).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				err := ct.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, EndStream: true,
					BlockFragment: buf.Bytes()})
				if err != nil || stream == 3 {
					return err
				}
			}
		}
	}
	ct.run()
	return frames, prioritized
}

// cut is how a block of total bytes of payload, priority of them the HEADERS
// frame's priority, is cut when each frame carries at most limit: as much as
// each can, and at least a byte of the block in the first.
func cut(total, limit, priority int) []int {
	block := total - priority
	first := min(block, max(limit-priority, 1))
	out := []int{first + priority}
	for block -= first; block > 0; block -= min(block, limit) {
		out = append(out, min(block, limit))
	}
	return out
}

func TestAHeaderBlockIsCutIntoFramesNoLargerThanAllowed(t *testing.T) {
	big := &PriorityParam{Exclusive: true, Weight: 255}
	none := &PriorityParam{}
	peerMiB := Setting{ID: SettingMaxFrameSize, Val: 1 << 20}
	for _, tc := range []struct {
		name     string
		limit    uint32
		priority *PriorityParam
		n        int
		settings []Setting
		// cut is the most a frame carries, and priority the HEADERS
		// frame's.
		cut, priority5 int
		frames         int
	}{
		// A full fragment and the priority made a HEADERS frame of 16,389
		// bytes, which a peer allowing 16,384 refuses.
		{"the peer's 16,384, with a priority", 0, nil, 40000, nil, 16384, 5, 2},
		{"the peer's 16,384, without", 0, none, 40000, nil, 16384, 0, 2},
		{"Chrome's 16,374, the peer allowing 16,384", 16374, big, 40000, nil, 16374, 5, 2},
		{"Chrome's 16,374, the peer allowing 1 MiB", 16374, big, 80000, []Setting{peerMiB}, 16374, 5, 4},
		{"a limit above the peer's", 1 << 20, big, 40000, nil, 16384, 5, 2},
		{"the peer's 1 MiB, unlimited", 0, big, 80000, []Setting{peerMiB}, 1 << 20, 5, 1},
		{"a block that fits", 16374, big, 100, nil, 16374, 5, 1},
		{"a limit that leaves no room but a byte", 3, big, 10, nil, 3, 5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames, prioritized := headerFrames(t, tc.limit, tc.priority, tc.n, tc.settings...)
			total := 0
			for _, f := range frames {
				total += f
			}
			want := cut(total, tc.cut, tc.priority5)
			if !slices.Equal(frames, want) || prioritized != (tc.priority5 == 5) || tc.frames > 0 && len(frames) != tc.frames {
				t.Errorf("frames %v (priority %v), want %v", frames, prioritized, want)
			}
		})
	}
}
