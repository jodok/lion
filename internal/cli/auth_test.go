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
		".linkedin.com\tTRUE\t/\tFALSE\t1999999999\tlidc\tb2\n"
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
