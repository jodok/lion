package cli

import (
	"reflect"
	"testing"
)

func TestParseCookieHeader(t *testing.T) {
	got := parseCookieHeader(`li_at=abc123; JSESSIONID="ajax:987"; bcookie="v=2&xyz"; lidc=b2`)
	want := map[string]string{
		"li_at":      "abc123",
		"JSESSIONID": `"ajax:987"`,
		"bcookie":    `"v=2&xyz"`,
		"lidc":       "b2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCookieHeader = %#v, want %#v", got, want)
	}
}

func TestParseCookieHeaderEmpty(t *testing.T) {
	if got := parseCookieHeader(""); len(got) != 0 {
		t.Errorf("parseCookieHeader(\"\") = %#v, want empty", got)
	}
	// Trailing semicolon / stray whitespace shouldn't produce a bogus entry.
	if got := parseCookieHeader(" li_at=x ; "); !reflect.DeepEqual(got, map[string]string{"li_at": "x"}) {
		t.Errorf("parseCookieHeader with trailing ';' = %#v", got)
	}
}

func TestParseCookieHeaderStripsCookiePrefix(t *testing.T) {
	// A header line copied straight from DevTools keeps its literal
	// "Cookie:" prefix; without stripping it, the first cookie name would
	// become "Cookie: li_at" and login's li_at check would fail.
	want := map[string]string{"li_at": "abc", "JSESSIONID": `"ajax:1"`}
	for _, in := range []string{
		`Cookie: li_at=abc; JSESSIONID="ajax:1"`,
		`cookie:li_at=abc; JSESSIONID="ajax:1"`,
		`COOKIE:  li_at=abc; JSESSIONID="ajax:1"`,
	} {
		if got := parseCookieHeader(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseCookieHeader(%q) = %#v, want %#v", in, got, want)
		}
	}
	// A cookie legitimately *named* "cookie" must not be mistaken for the
	// prefix (the prefix ends in a colon, "cookie=" does not).
	if got := parseCookieHeader("cookie=value; li_at=x"); !reflect.DeepEqual(got, map[string]string{"cookie": "value", "li_at": "x"}) {
		t.Errorf("parseCookieHeader mishandled a cookie named 'cookie': %#v", got)
	}
}

func TestParseCookiesFileHeaderLine(t *testing.T) {
	got := parseCookiesFile("li_at=abc; JSESSIONID=\"ajax:1\"\n")
	want := map[string]string{"li_at": "abc", "JSESSIONID": `"ajax:1"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCookiesFile(header line) = %#v, want %#v", got, want)
	}
}

func TestParseCookiesFileNetscape(t *testing.T) {
	netscape := "# Netscape HTTP Cookie File\n" +
		".linkedin.com\tTRUE\t/\tTRUE\t1999999999\tli_at\tabc123\n" +
		"#HttpOnly_.linkedin.com\tTRUE\t/\tTRUE\t1999999999\tJSESSIONID\t\"ajax:987\"\n" +
		"www.linkedin.com\tFALSE\t/\tFALSE\t1999999999\tlidc\tb2\n"
	got := parseCookiesFile(netscape)
	want := map[string]string{
		"li_at":      "abc123",
		"JSESSIONID": `"ajax:987"`,
		"lidc":       "b2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCookiesFile(netscape) = %#v, want %#v", got, want)
	}
}

func TestParseNetscapeCookiesDropsForeignDomains(t *testing.T) {
	// cookiesToFHTTP rewrites every cookie to .linkedin.com, so a general
	// cookies.txt export must not leak other sites' cookies into the jar.
	netscape := "# Netscape HTTP Cookie File\n" +
		".linkedin.com\tTRUE\t/\tTRUE\t1999999999\tli_at\tkeep\n" +
		".evil.com\tTRUE\t/\tTRUE\t1999999999\tsession\tsecret\n" +
		".notlinkedin.com\tTRUE\t/\tTRUE\t1999999999\tli_at\tforeign\n" +
		"sub.linkedin.com\tTRUE\t/\tTRUE\t1999999999\tlidc\tkeep2\n"
	got := parseNetscapeCookies(netscape)
	want := map[string]string{"li_at": "keep", "lidc": "keep2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNetscapeCookies dropped/kept wrong domains: %#v, want %#v", got, want)
	}
}

func TestResolveLoginCookiesLiAtJsessionidQuoting(t *testing.T) {
	// --jsessionid without quotes must come out quoted (LinkedIn's wire
	// format), and with quotes must not be double-quoted.
	got, err := resolveLoginCookies("", "", "abc", "ajax:1", true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"li_at": "abc", "JSESSIONID": `"ajax:1"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies(unquoted jsession) = %#v, want %#v", got, want)
	}

	got, err = resolveLoginCookies("", "", "abc", `"ajax:1"`, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies(quoted jsession) = %#v, want %#v", got, want)
	}
}

func TestResolveLoginCookiesPrefersCookiesFlag(t *testing.T) {
	got, err := resolveLoginCookies(`li_at=x; JSESSIONID="ajax:2"; lidc=y`, "", "ignored-li-at", "ignored-jsession", true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"li_at": "x", "JSESSIONID": `"ajax:2"`, "lidc": "y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies with --cookies = %#v, want %#v", got, want)
	}
}

// TestResolveLoginCookiesNormalizesJSessionAcrossPaths pins the Fix-1
// invariant: no matter the input path or how the pasted JSESSIONID was
// quoted, resolveLoginCookies returns it wrapped in exactly one pair of
// double quotes.
func TestResolveLoginCookiesNormalizesJSessionAcrossPaths(t *testing.T) {
	const wantJSession = `"ajax:9"`
	cases := []struct {
		name                        string
		cookiesFlag, liAt, jsession string
	}{
		{"cookies flag unquoted", "li_at=a; JSESSIONID=ajax:9", "", ""},
		{"cookies flag quoted", `li_at=a; JSESSIONID="ajax:9"`, "", ""},
		{"cookies flag doubled quotes", `li_at=a; JSESSIONID=""ajax:9""`, "", ""},
		{"flags unquoted", "", "a", "ajax:9"},
		{"flags quoted", "", "a", `"ajax:9"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLoginCookies(tc.cookiesFlag, "", tc.liAt, tc.jsession, true)
			if err != nil {
				t.Fatal(err)
			}
			if got["JSESSIONID"] != wantJSession {
				t.Errorf("JSESSIONID = %q, want %q (exactly one quote pair)", got["JSESSIONID"], wantJSession)
			}
		})
	}
}
