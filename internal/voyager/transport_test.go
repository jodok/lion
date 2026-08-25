package voyager

import (
	"net/http"
	"testing"
)

func TestCookieHeader(t *testing.T) {
	// Stable (sorted) order; JSESSIONID keeps its stored quotes.
	got := cookieHeader(map[string]string{
		"li_at":      "abc",
		"JSESSIONID": `"ajax:123"`,
	})
	want := `JSESSIONID="ajax:123"; li_at=abc`
	if got != want {
		t.Errorf("cookieHeader = %q, want %q", got, want)
	}
	if cookieHeader(nil) != "" {
		t.Errorf("empty cookies should yield empty header")
	}
}

func TestWithCookiesUpdatesCsrf(t *testing.T) {
	// WithCookies replacing JSESSIONID must update the derived csrf token.
	c := New("li_at", `"old"`, WithCookies(map[string]string{"JSESSIONID": `"new:tok"`}))
	if c.csrf != "new:tok" {
		t.Errorf("csrf = %q, want new:tok", c.csrf)
	}
	if c.cookies["li_at"] != "li_at" {
		t.Errorf("li_at cookie should be preserved, got %q", c.cookies["li_at"])
	}
}

// TestJSESSIONIDQuotingSurvivesSnapshot is an end-to-end guard on the one
// detail cookie writeback can get silently wrong. LinkedIn sends JSESSIONID
// wrapped in quotes and expects them back on the wire, and the csrf-token
// header is derived by stripping them — but both net/http and fhttp parse a
// quoted value by moving the quotes into Cookie.Quoted rather than leaving
// them in Value. Reading a jar back naively therefore drops them, and the
// stored credential degrades a little more on every command until csrf stops
// matching, with no error at the point the damage is done.
func TestJSESSIONIDQuotingSurvivesSnapshot(t *testing.T) {
	hdr := http.Header{}
	hdr.Add("Set-Cookie", `JSESSIONID="ajax:1234567890"; Path=/; Domain=.linkedin.com`)
	resp := http.Response{Header: hdr}
	got := cookiesToMap(resp.Cookies())
	if got["JSESSIONID"] != `"ajax:1234567890"` {
		t.Fatalf("JSESSIONID = %q, want it to keep its wire quotes", got["JSESSIONID"])
	}
	// An unquoted cookie must not gain quotes on the way back out.
	if got2 := cookiesToMap((&http.Response{Header: mustHeader(`li_at=AQEDAT; Path=/`)}).Cookies()); got2["li_at"] != "AQEDAT" {
		t.Fatalf("li_at = %q, want it unchanged", got2["li_at"])
	}
}

func mustHeader(setCookie string) http.Header {
	h := http.Header{}
	h.Add("Set-Cookie", setCookie)
	return h
}
