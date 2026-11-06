package cookiejar

import (
	"strings"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
)

// hostOnlyStep is a Set-Cookie line from the host of from, or, with no line,
// what the jar sends to the URL of query, as name=value pairs and each
// cookie's Domain.
type hostOnlyStep struct {
	set     string
	query   string
	want    string
	domains string
}

// A cookie its host's alone and one its domain's of the same name, domain and
// path are two cookies where the jar keeps them apart, as Chrome 156 keeps
// them (measured at example.test: a host-only a and an a of
// Domain=example.test both sent, each replaced and removed by its own kind),
// and one where it does not, as RFC 6265 has it. A Domain naming a host below
// the apex is the host's domain too.
func TestAHostOnlyCookieIsKeptApartFromItsDomainsWhereTheJarSays(t *testing.T) {
	const apex, www, sub = "http://example.test/", "http://www.example.test/", "http://sub.example.test/"
	apart := []hostOnlyStep{
		{set: "a=host"},
		{set: "a=dom; Domain=example.test"},
		{query: apex, want: "a=host a=dom", domains: "example.test .example.test"},
		{query: sub, want: "a=dom", domains: ".example.test"},
		{set: "a=dom2; Domain=example.test"},
		{query: apex, want: "a=host a=dom2"},
		{set: "a=host2"},
		{query: apex, want: "a=host2 a=dom2"},
		{set: "a=; Max-Age=0"},
		{query: apex, want: "a=dom2"},
		{query: sub, want: "a=dom2"},
		{set: "a=h3"},
		{set: "a=; Domain=example.test; Max-Age=0"},
		{query: apex, want: "a=h3", domains: "example.test"},
		{query: sub, want: ""},
	}
	together := []hostOnlyStep{
		{set: "a=host"},
		{set: "a=dom; Domain=example.test"},
		{query: apex, want: "a=dom", domains: "example.test"},
		{query: sub, want: "a=dom"},
		{set: "a=host2"},
		{query: apex, want: "a=host2"},
		{query: sub, want: ""},
		{set: "a=; Domain=example.test; Max-Age=0"},
		{query: apex, want: ""},
	}
	for _, tc := range []struct {
		from, set string
		apart     bool
		steps     []hostOnlyStep
	}{
		{apex, "", true, apart},
		{apex, "", false, together},
		{www, "b=dom; Domain=www.example.test", true, []hostOnlyStep{{query: www, want: "b=host b=dom",
			domains: "www.example.test .www.example.test"}}},
		{www, "b=dom; Domain=www.example.test", false, []hostOnlyStep{{query: www, want: "b=dom",
			domains: "www.example.test"}}},
	} {
		jar, err := New(&Options{PublicSuffixList: testPSL{}, HostOnlyApart: tc.apart})
		if err != nil {
			t.Fatal(err)
		}
		now := tNow
		steps := tc.steps
		if tc.set != "" {
			steps = append([]hostOnlyStep{{set: "b=host"}, {set: tc.set}}, steps...)
		}
		for i, step := range steps {
			now = now.Add(time.Second)
			if step.set != "" {
				cookies := (&http.Response{Header: http.Header{"Set-Cookie": {step.set}}}).Cookies()
				jar.setCookies(mustParseURL(tc.from), cookies, now)
				continue
			}
			var pairs, domains []string
			for _, c := range jar.cookies(mustParseURL(step.query), now) {
				pairs = append(pairs, c.Name+"="+c.Value)
				domains = append(domains, c.Domain)
			}
			if got := strings.Join(pairs, " "); got != step.want {
				t.Errorf("%s apart %v, step %d, %s: %q, want %q", tc.from, tc.apart, i, step.query, got, step.want)
			}
			if got := strings.Join(domains, " "); step.domains != "" && got != step.domains {
				t.Errorf("%s apart %v, step %d, %s: domains %q, want %q", tc.from, tc.apart, i, step.query, got,
					step.domains)
			}
		}
	}
}
