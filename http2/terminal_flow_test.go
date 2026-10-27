package http2

import (
	"fmt"
	http "github.com/igor-lemon/fhttp"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTerminalFlowErrorDoesNotOrphanResponseBody(t *testing.T) {
	for _, window := range []uint32{0, 1} {
		for _, terminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("window=%d/terminal=%v", window, terminal), func(t *testing.T) {
				ct := newClientTester(t)
				ct.tr.Settings = map[SettingID]uint32{SettingInitialWindowSize: window}
				ct.tr.SettingsOrder = []SettingID{SettingInitialWindowSize}
				ct.client = func() error {
					req, _ := http.NewRequest("GET", "https://owned.test/", nil)
					res, err := ct.tr.RoundTrip(req)
					if err != nil {
						return err
					}
					defer res.Body.Close()
					result := make(chan error, 1)
					go func() {
						body, e := io.ReadAll(res.Body)
						if len(body) != 0 {
							e = fmt.Errorf("illegal bytes exposed: %q", body)
						}
						result <- e
					}()
					select {
					case e := <-result:
						if e == nil || !strings.Contains(e.Error(), "FLOW_CONTROL_ERROR") {
							return fmt.Errorf("body error=%v", e)
						}
						return nil
					case <-time.After(time.Second):
						return fmt.Errorf("terminal flow error orphaned a response reader")
					}
				}
				ct.server = func() error {
					ct.greet()
					h, err := ct.firstHeaders()
					if err != nil {
						return err
					}
					if err = ct.fr.WriteHeaders(HeadersFrameParam{StreamID: h.StreamID, BlockFragment: []byte{0x88}, EndHeaders: true}); err != nil {
						return err
					}
					return ct.fr.WriteData(h.StreamID, terminal, make([]byte, int(window)+1))
				}
				ct.run()
			})
		}
	}
}
