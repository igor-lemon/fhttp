package http2

import (
	"bytes"
	"fmt"
	"golang.org/x/net/http2/hpack"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptrace"
)

func TestHeaderOrder(t *testing.T) {
	req, err := http.NewRequest("POST", "https://www.httpbin.org/headers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = http.Header{
		"sec-ch-ua":        {"\" Not;A Brand\";v=\"99\", \"Google Chrome\";v=\"91\", \"Chromium\";v=\"91\""},
		"accept":           {"*/*"},
		"x-requested-with": {"XMLHttpRequest"},
		"sec-ch-ua-mobile": {"?0"},
		"user-agent":       {"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.93 Safari/537.36\", \"I shouldn't be here"},
		"content-type":     {"application/json"},
		"origin":           {"https://www.size.co.uk/"},
		"sec-fetch-site":   {"same-origin"},
		"sec-fetch-mode":   {"cors"},
		"sec-fetch-dest":   {"empty"},
		"accept-language":  {"en-US,en;q=0.9"},
		"accept-encoding":  {"gzip, deflate, br"},
		"referer":          {"https://www.size.co.uk/product/white-jordan-air-1-retro-high/16077886/"},
		http.HeaderOrderKey: {
			"sec-ch-ua",
			"accept",
			"x-requested-with",
			"sec-ch-ua-mobile",
			"user-agent",
			"content-type",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
		},
		http.PHeaderOrderKey: {
			":method",
			":authority",
			":scheme",
			":path",
		},
	}
	var b []byte
	buf := bytes.NewBuffer(b)
	err = req.Header.Write(buf)
	if err != nil {
		t.Fatal(err)
	}
	arr := strings.Split(buf.String(), "\n")
	var hdrs []string
	for _, v := range arr {
		a := strings.Split(v, ":")
		if a[0] == "" {
			continue
		}
		hdrs = append(hdrs, a[0])
	}

	for i := range req.Header[http.HeaderOrderKey] {
		if hdrs[i] != req.Header[http.HeaderOrderKey][i] {
			t.Errorf("want: %s\ngot: %s\n", req.Header[http.HeaderOrderKey][i], hdrs[i])
		}
	}
}

func TestHeaderOrder2(t *testing.T) {
	hk := ""
	trace := &httptrace.ClientTrace{
		WroteHeaderField: func(key string, values []string) {
			hk += key + " "
		},
	}
	req, err := http.NewRequest("GET", "https://owned.example/headers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("experience", "pain")
	req.Header.Add("grind", "harder")
	req.Header.Add("live", "mas")
	req.Header[http.HeaderOrderKey] = []string{"grind", "experience", "live"}
	req.Header[http.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	assertOwnedHeaderOrder(t, req, []string{":method", ":authority", ":scheme", ":path", "grind", "experience", "live", "accept-encoding", "user-agent"})
	if !strings.EqualFold(hk, ":method :authority :scheme :path grind experience live accept-encoding user-agent ") {
		t.Fatalf("trace order=%q", hk)
	}
}

func TestHeaderOrder3(t *testing.T) {
	req, err := http.NewRequest("GET", "https://owned.example/headers", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = http.Header{
		http.HeaderOrderKey: {
			"sec-ch-ua",
			"accept",
			"x-requested-with",
			"sec-ch-ua-mobile",
			"user-agent",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"referer",
			"accept-encoding",
			"accept-language",
			"cookie",
		},
	}
	req.Header.Add("accept", "text / html, application/xhtml + xml, application / xml;q = 0.9, image/avif, image/webp, image/apng, * /*;q=0.8,application/signed-exchange;v=b3;q=0.9")
	req.Header.Add("accept-encoding", "gzip, deflate, br")
	req.Header.Add("accept-language", "en-GB,en-US;q=0.9,en;q=0.8")
	req.Header.Add("cache-control", "no-cache")
	req.Header.Add("pragma", "no-cache")
	req.Header.Add("referer", "https://www.offspring.co.uk/")
	req.Header.Add("sec-ch-ua", `" Not A;Brand"; v = "99", "Chromium"; v = "90", "Google Chrome"; v = "90"`)
	req.Header.Add("sec-ch-ua-mobile", "?0")
	req.Header.Add("sec-fetch-dest", "document")
	req.Header.Add("sec-fetch-mode", "navigate")
	req.Header.Add("sec-fetch-site", "same-origin")
	req.Header.Add("sec-fetch-user", "?1")
	req.Header.Add("upgrade-insecure-requests", "1")
	req.Header.Add("user-agent", "Mozilla")
	var hdrs string
	trace := &httptrace.ClientTrace{WroteHeaderField: func(key string, value []string) {
		hdrs += key + "\n"
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	assertOwnedHeaderOrder(t, req, []string{":authority", ":method", ":path", ":scheme", "sec-ch-ua", "accept", "sec-ch-ua-mobile", "user-agent", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "cache-control", "pragma", "sec-fetch-user", "upgrade-insecure-requests"})
	if strings.TrimSpace(hdrs) == "" {
		t.Fatal("no header trace")
	}
}

// The oracle decodes actual HPACK received by an owned peer. It never builds
// its result from expected profile data or trusts only the client trace hook.
func assertOwnedHeaderOrder(t *testing.T, req *http.Request, want []string) {
	t.Helper()
	original := req.Header.Clone()
	ct := newClientTester(t)
	ct.sc.SetDeadline(time.Now().Add(5 * time.Second))
	ct.cc.SetDeadline(time.Now().Add(5 * time.Second))
	ct.client = func() error {
		response, err := ct.tr.RoundTrip(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		if response.StatusCode != 200 {
			return fmt.Errorf("status=%d", response.StatusCode)
		}
		if !reflect.DeepEqual(req.Header, original) {
			return fmt.Errorf("caller header changed")
		}
		return err
	}
	ct.server = func() error {
		ct.greet()
		for {
			frame, err := ct.fr.ReadFrame()
			if err != nil {
				return err
			}
			switch f := frame.(type) {
			case *SettingsFrame, *WindowUpdateFrame:
			case *HeadersFrame:
				block := append([]byte(nil), f.HeaderBlockFragment()...)
				ended := f.HeadersEnded()
				for !ended {
					next, err := ct.fr.ReadFrame()
					if err != nil {
						return err
					}
					cont, ok := next.(*ContinuationFrame)
					if !ok || cont.StreamID != f.StreamID {
						return fmt.Errorf("unexpected continuation %T", next)
					}
					block = append(block, cont.HeaderBlockFragment()...)
					ended = cont.HeadersEnded()
				}
				fields, err := hpack.NewDecoder(4096, nil).DecodeFull(block)
				if err != nil {
					return err
				}
				var names []string
				values := make(map[string][]string)
				for _, field := range fields {
					names = append(names, field.Name)
					values[field.Name] = append(values[field.Name], field.Value)
				}
				if !reflect.DeepEqual(names, want) {
					return fmt.Errorf("wire order=%v,want %v", names, want)
				}
				if !reflect.DeepEqual(values[":method"], []string{req.Method}) || !reflect.DeepEqual(values[":authority"], []string{req.URL.Host}) || !reflect.DeepEqual(values[":path"], []string{req.URL.RequestURI()}) {
					return fmt.Errorf("wrong pseudo-header values: %v", values)
				}
				for key, v := range req.Header {
					if key == http.HeaderOrderKey || key == http.PHeaderOrderKey {
						continue
					}
					if !reflect.DeepEqual(values[strings.ToLower(key)], v) {
						return fmt.Errorf("wire %s=%v,want %v", key, values[strings.ToLower(key)], v)
					}
				}
				return ct.fr.WriteHeaders(HeadersFrameParam{StreamID: f.StreamID, EndHeaders: true, EndStream: true, BlockFragment: []byte{0x88}})
			default:
				return fmt.Errorf("unexpected frame %T", f)
			}
		}
	}
	ct.run()
}
