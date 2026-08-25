package voyager

import "testing"

// TestNewChromeTransportImplementsTransport is a construction/interface
// test only — it must never make a live LinkedIn request (a real session
// was invalidated by over-probing during development; see DESIGN.md §3.3).
// It just verifies the Chrome-impersonating transport builds successfully
// and satisfies the Transport seam so it can be swapped in via
// voyager.WithTransport / voyager.WithCookies without touching client.go.
func TestNewChromeTransportImplementsTransport(t *testing.T) {
	tr, err := NewChromeTransport(map[string]string{
		"li_at":      "test-li-at",
		"JSESSIONID": `"ajax:test"`,
		"bcookie":    "test-bcookie",
		"lidc":       "test-lidc",
	})
	if err != nil {
		t.Fatalf("NewChromeTransport: %v", err)
	}
	if tr == nil {
		t.Fatal("NewChromeTransport returned a nil Transport")
	}
	var _ Transport = tr
}

// TestNewChromeTransportEmptyCookies confirms construction doesn't require
// cookies up front (e.g. WithCookies may be applied after New but before
// any request); the jar just starts empty.
func TestNewChromeTransportEmptyCookies(t *testing.T) {
	if _, err := NewChromeTransport(nil); err != nil {
		t.Fatalf("NewChromeTransport(nil): %v", err)
	}
}

// TestCookiesToFHTTP verifies the jar-seeding conversion: values are used
// verbatim (including JSESSIONID's surrounding quotes) and cookies are
// scoped to the whole linkedin.com domain, per DESIGN.md §3.3.
func TestCookiesToFHTTP(t *testing.T) {
	got := cookiesToFHTTP(map[string]string{
		"li_at":      "abc",
		"JSESSIONID": `"ajax:123"`,
	})
	if len(got) != 2 {
		t.Fatalf("got %d cookies, want 2", len(got))
	}
	byName := map[string]string{}
	for _, c := range got {
		if c.Domain != ".linkedin.com" {
			t.Errorf("cookie %s domain = %q, want .linkedin.com", c.Name, c.Domain)
		}
		if c.Path != "/" {
			t.Errorf("cookie %s path = %q, want /", c.Name, c.Path)
		}
		if c.Quoted {
			t.Errorf("cookie %s Quoted = true, want false (value already carries any quotes)", c.Name)
		}
		byName[c.Name] = c.Value
	}
	if byName["JSESSIONID"] != `"ajax:123"` {
		t.Errorf("JSESSIONID value = %q, want quotes preserved", byName["JSESSIONID"])
	}
	if byName["li_at"] != "abc" {
		t.Errorf("li_at value = %q, want abc", byName["li_at"])
	}
}
