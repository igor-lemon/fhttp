// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// dataFrames are the payload lengths of the DATA frames of a request's body
// of n bytes, as a transport whose MaxDataFramePayload is limit wrote it to a
// server whose windows took all of it and that said settings, after a first
// request the server's SETTINGS and windows came before; and which of them
// ended the stream.
func dataFrames(t *testing.T, limit uint32, n int, settings ...Setting) ([]int, []bool) {
	t.Helper()
	ct := newClientTester(t)
	ct.tr.MaxDataFramePayload = limit
	var frames []int
	var ends []bool
	ct.client = func() error {
		for _, method := range []string{"GET", "POST"} {
			var body io.Reader
			if method == "POST" {
				body = strings.NewReader(strings.Repeat("a", n))
			}
			req, _ := http.NewRequest(method, "https://dummy.tld/", body)
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
		ct.greet(append(settings, Setting{ID: SettingInitialWindowSize, Val: 1 << 20})...)
		if err := ct.fr.WriteWindowUpdate(0, 1<<20); err != nil {
			return err
		}
		var buf bytes.Buffer
		_ = hpack.NewEncoder(&buf).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		answer := func(stream uint32) error {
			return ct.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, EndStream: true,
				BlockFragment: buf.Bytes()})
		}
		for {
			f, err := ct.readNonSettingsFrame()
			if err != nil {
				return err
			}
			switch f := f.(type) {
			case *HeadersFrame:
				if f.StreamID == 1 {
					if err := answer(1); err != nil {
						return err
					}
				}
			case *DataFrame:
				frames, ends = append(frames, len(f.Data())), append(ends, f.StreamEnded())
				if f.StreamEnded() {
					return answer(f.StreamID)
				}
			}
		}
	}
	ct.run()
	return frames, ends
}

func TestABodyIsSentInDATAFramesNoLargerThanAllowed(t *testing.T) {
	peerMiB := Setting{ID: SettingMaxFrameSize, Val: 1 << 20}
	chrome := []int{16375, 16375, 16375, 16375, 36}
	for _, tc := range []struct {
		name     string
		limit    uint32
		n        int
		settings []Setting
		want     []int
	}{
		{"Chrome's 16,375, the peer allowing 16,384", 16375, 65536, nil, chrome},
		{"Chrome's 16,375, the peer allowing 1 MiB", 16375, 65536, []Setting{peerMiB}, chrome},
		{"the peer's 16,384", 0, 65536, nil, []int{16384, 16384, 16384, 16384}},
		{"the peer's 1 MiB", 0, 65536, []Setting{peerMiB}, []int{65536}},
		{"a limit above the peer's", 1 << 20, 65536, nil, []int{16384, 16384, 16384, 16384}},
		{"a body smaller than a frame", 16375, 1000, nil, []int{1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames, ends := dataFrames(t, tc.limit, tc.n, tc.settings...)
			last := slices.Index(ends, true)
			if !slices.Equal(frames, tc.want) || last != len(frames)-1 {
				t.Errorf("frames %v, the stream ended at %d; want %v", frames, last, tc.want)
			}
		})
	}
}
