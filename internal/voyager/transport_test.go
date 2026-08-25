package voyager

import "testing"

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
