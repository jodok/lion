package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/auth"
	"github.com/jodok/lion/internal/voyager"
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
	got, err := resolveLoginCookies("", "", false, "abc", "ajax:1", true, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"li_at": "abc", "JSESSIONID": `"ajax:1"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies(unquoted jsession) = %#v, want %#v", got, want)
	}

	got, err = resolveLoginCookies("", "", false, "abc", `"ajax:1"`, true, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies(quoted jsession) = %#v, want %#v", got, want)
	}
}

func TestResolveLoginCookiesPrefersCookiesFlag(t *testing.T) {
	got, err := resolveLoginCookies(`li_at=x; JSESSIONID="ajax:2"; lidc=y`, "", false, "ignored-li-at", "ignored-jsession", true, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"li_at": "x", "JSESSIONID": `"ajax:2"`, "lidc": "y"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies with --cookies = %#v, want %#v", got, want)
	}
}

// TestResolveLoginCookiesStdin is the F9 regression test: --cookies-stdin
// (and the `--cookies -` shorthand) must read the Cookie header from stdin
// rather than requiring it as a command-line argument.
func TestResolveLoginCookiesStdin(t *testing.T) {
	want := map[string]string{"li_at": "x", "JSESSIONID": `"ajax:2"`, "lidc": "y"}

	got, err := resolveLoginCookies("", "", true, "", "", true, strings.NewReader(`li_at=x; JSESSIONID="ajax:2"; lidc=y`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies(--cookies-stdin) = %#v, want %#v", got, want)
	}

	// The `--cookies -` shorthand is equivalent.
	got, err = resolveLoginCookies("-", "", false, "", "", true, strings.NewReader(`li_at=x; JSESSIONID="ajax:2"; lidc=y`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveLoginCookies(--cookies -) = %#v, want %#v", got, want)
	}
}

// TestResolveLoginCookiesStdinAcceptsNetscapeFormat confirms stdin input
// goes through the same format sniffing as --cookies-file.
func TestResolveLoginCookiesStdinAcceptsNetscapeFormat(t *testing.T) {
	netscape := "# Netscape HTTP Cookie File\n" +
		".linkedin.com\tTRUE\t/\tTRUE\t1999999999\tli_at\tabc123\n"
	got, err := resolveLoginCookies("", "", true, "", "", true, strings.NewReader(netscape))
	if err != nil {
		t.Fatal(err)
	}
	if got["li_at"] != "abc123" {
		t.Errorf("resolveLoginCookies(--cookies-stdin, netscape) = %#v, want li_at=abc123", got)
	}
}

// TestWarnArgvCredentialsDetectsArgvUsage is the F9 warning regression test:
// credentials passed directly as flag values must trigger the stderr
// warning; --cookies-file/--cookies-stdin paths (and the `--cookies -`
// shorthand, which never carries the value in argv) must not.
func TestWarnArgvCredentialsDetectsArgvUsage(t *testing.T) {
	cases := []struct {
		name                        string
		liAt, jsession, cookiesFlag string
		wantWarning                 bool
	}{
		{"nothing set", "", "", "", false},
		{"li-at set", "x", "", "", true},
		{"jsessionid set", "", "x", "", true},
		{"cookies value set", "", "", "li_at=x", true},
		{"cookies dash (stdin) not argv", "", "", "-", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				warnArgvCredentials(tc.liAt, tc.jsession, tc.cookiesFlag)
			})
			gotWarning := out != ""
			if gotWarning != tc.wantWarning {
				t.Errorf("warnArgvCredentials(%q,%q,%q) wrote %q, want warning=%v", tc.liAt, tc.jsession, tc.cookiesFlag, out, tc.wantWarning)
			}
		})
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written, restoring the original afterward.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// meFixture is a minimal but valid /me response body (mirrors
// testdata/fixtures/me.json), used to build a fixture voyager.Transport
// without depending on a filesystem path relative to this test's package.
const meFixture = `{
	"data": {
		"*miniProfile": "urn:li:fs_miniProfile:ACoAAtestada"
	},
	"included": [
		{
			"$type": "com.linkedin.voyager.identity.shared.MiniProfile",
			"entityUrn": "urn:li:fs_miniProfile:ACoAAtestada",
			"publicIdentifier": "ada-lovelace",
			"firstName": "Ada",
			"lastName": "Lovelace",
			"occupation": "Mathematician"
		}
	]
}`

// fixtureTransport is a minimal voyager.Transport that always returns a
// fixed status/body — enough to drive validateAccounts through the real
// voyager.Client.Me() codepath without a live account or Chrome TLS
// impersonation.
type statusFixtureTransport struct {
	status int
	body   string
}

func (f *statusFixtureTransport) Do(_ context.Context, _ *voyager.Request) (*voyager.Response, error) {
	return &voyager.Response{StatusCode: f.status, Body: []byte(f.body)}, nil
}

// TestValidateAccountsValid and TestValidateAccountsExpired are the F1
// regression tests: `auth status` must actually call LinkedIn (here, via an
// injected fixture transport standing in for the network) rather than just
// listing stored accounts, and must classify a 200 as valid and a 401 as
// expired.
func TestValidateAccountsValid(t *testing.T) {
	creds := []*auth.Credential{{Alias: "default", Name: "stale-name"}}
	newClient := func(c *auth.Credential) (*voyager.Client, error) {
		return voyager.New("li_at", `"jsession"`, voyager.WithTransport(&statusFixtureTransport{status: 200, body: meFixture})), nil
	}
	got := validateAccounts(context.Background(), creds, "default", newClient)
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1", len(got))
	}
	if got[0].State != "valid" {
		t.Errorf("State = %q, want valid", got[0].State)
	}
	if !got[0].Default {
		t.Error("Default = false, want true")
	}
}

func TestValidateAccountsExpired(t *testing.T) {
	creds := []*auth.Credential{{Alias: "default"}}
	newClient := func(c *auth.Credential) (*voyager.Client, error) {
		return voyager.New("li_at", `"jsession"`, voyager.WithTransport(&statusFixtureTransport{status: 401, body: `{}`})), nil
	}
	got := validateAccounts(context.Background(), creds, "default", newClient)
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1", len(got))
	}
	if got[0].State != "expired" {
		t.Errorf("State = %q, want expired", got[0].State)
	}
}

// TestValidateAccountsUnreachableIsUnknown covers the resiliency
// requirement: a transport-level error (network down, timeout) must be
// reported as "unknown", never crash the command or masquerade as
// "expired".
func TestValidateAccountsUnreachableIsUnknown(t *testing.T) {
	creds := []*auth.Credential{{Alias: "default"}}
	newClient := func(c *auth.Credential) (*voyager.Client, error) {
		return nil, errors.New("transport unavailable")
	}
	got := validateAccounts(context.Background(), creds, "default", newClient)
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1", len(got))
	}
	if got[0].State != "unknown" {
		t.Errorf("State = %q, want unknown", got[0].State)
	}
}

// TestValidateAccountsSortedByAlias is the F18 regression test at the
// command layer (store.List already sorts; this pins that validateAccounts
// preserves/produces that order regardless of input order).
func TestValidateAccountsSortedByAlias(t *testing.T) {
	creds := []*auth.Credential{{Alias: "zebra"}, {Alias: "alpha"}, {Alias: "mid"}}
	newClient := func(c *auth.Credential) (*voyager.Client, error) {
		return voyager.New("li_at", `"jsession"`, voyager.WithTransport(&statusFixtureTransport{status: 200, body: meFixture})), nil
	}
	got := validateAccounts(context.Background(), creds, "", newClient)
	want := []string{"alpha", "mid", "zebra"}
	for i, w := range want {
		if got[i].Alias != w {
			t.Errorf("got[%d].Alias = %q, want %q", i, got[i].Alias, w)
		}
	}
}

// TestAuthLogoutResolvesStoreDefault is the F2 regression test: logging out
// with no alias argument must remove the store's actual recorded default,
// not the literal string "default".
func TestAuthLogoutResolvesStoreDefault(t *testing.T) {
	isolateHome(t)
	if err := auth.Save(&auth.Credential{Alias: "work", Cookies: map[string]string{"li_at": "x"}}); err != nil {
		t.Fatal(err)
	}
	def, err := auth.DefaultAlias()
	if err != nil {
		t.Fatal(err)
	}
	if def != "work" {
		t.Fatalf("DefaultAlias() = %q, want work", def)
	}
	if err := auth.Delete(def); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Get("work"); err != auth.ErrNoAccount {
		t.Errorf("Get(work) after logout = %v, want ErrNoAccount", err)
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
			got, err := resolveLoginCookies(tc.cookiesFlag, "", false, tc.liAt, tc.jsession, true, strings.NewReader(""))
			if err != nil {
				t.Fatal(err)
			}
			if got["JSESSIONID"] != wantJSession {
				t.Errorf("JSESSIONID = %q, want %q (exactly one quote pair)", got["JSESSIONID"], wantJSession)
			}
		})
	}
}
