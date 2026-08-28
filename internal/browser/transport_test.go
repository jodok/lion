package browser

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/jodok/lion/internal/voyager"
)

// newTestBrowser launches a real headless Chromium against a throwaway
// profile, pointed at srv instead of LinkedIn, and loads the home page.
//
// It deliberately does not call Open: Open also asserts that the profile is
// signed in, and most of what needs testing here — issuing a request from
// the page, reading the csrf cookie, recognising a session-less profile —
// either does not need a session or is specifically about its absence.
// Tests that want Open's check call it themselves.
//
// It skips rather than fails when no Chrome is installed: the transport
// cannot be exercised without a browser, and a machine without one should
// not turn a green suite red.
func newTestBrowser(t *testing.T, srvURL string) *Browser {
	t.Helper()
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("no Chrome/Chromium installed; skipping browser transport test")
	}
	t.Setenv("LION_HOME", t.TempDir())

	orig := homeURL
	homeURL = srvURL + "/home"
	t.Cleanup(func() { homeURL = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	b, err := Launch(ctx, Options{Alias: "test"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(b.Close)
	if err := b.page.Context(ctx).Navigate(homeURL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := b.page.Context(ctx).WaitLoad(); err != nil {
		t.Fatalf("wait load: %v", err)
	}
	return b
}

// TestTransportDoIssuesRequestFromThePage is the core guarantee: a
// voyager.Request goes out as a same-origin fetch() from the loaded page and
// comes back as a voyager.Response carrying status, body, headers, and the
// post-redirect URL.
func TestTransportDoIssuesRequestFromThePage(t *testing.T) {
	var gotMethod, gotBody, gotCSRF, gotCookie string
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: `"ajax:42"`, Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>home</body></html>")
	})
	mux.HandleFunc("/api/thing", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCSRF = r.Header.Get("Csrf-Token")
		gotCookie = r.Header.Get("Cookie")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Marker", "seen")
		w.WriteHeader(201)
		io.WriteString(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)
	tr := b.Transport()

	resp, err := tr.Do(context.Background(), &voyager.Request{
		Method:  "POST",
		URL:     srv.URL + "/api/thing",
		Headers: map[string]string{"Csrf-Token": "ajax:42", "User-Agent": "should-be-dropped"},
		Body:    []byte(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if resp.StatusCode != 201 {
		t.Errorf("StatusCode = %d, want 201", resp.StatusCode)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("Body = %q", resp.Body)
	}
	if got := resp.Headers.Get("X-Marker"); got != "seen" {
		t.Errorf("response header X-Marker = %q, want %q", got, "seen")
	}
	if resp.FinalURL != srv.URL+"/api/thing" {
		t.Errorf("FinalURL = %q", resp.FinalURL)
	}
	if gotMethod != "POST" {
		t.Errorf("server saw method %q", gotMethod)
	}
	if gotBody != `{"hello":"world"}` {
		t.Errorf("server saw body %q", gotBody)
	}
	if gotCSRF != "ajax:42" {
		t.Errorf("server saw Csrf-Token %q", gotCSRF)
	}
	// The page's own cookie jar must ride along without lion supplying it —
	// that is the whole point of issuing the request from inside the page.
	if !strings.Contains(gotCookie, "JSESSIONID") {
		t.Errorf("server saw Cookie %q, want the page's JSESSIONID", gotCookie)
	}
	// User-Agent is a forbidden fetch header; the browser's own value must
	// win rather than lion's synthesized one leaking through.
	if strings.Contains(gotCookie, "should-be-dropped") {
		t.Errorf("synthesized User-Agent leaked into the request")
	}
}

// TestTransportDoReportsRedirectTarget covers what classifyRedirect depends
// on: a request that gets redirected must report where it actually landed,
// since that is how an expired session (bounced to the login wall) is
// detected now that Set-Cookie is invisible to page script.
func TestTransportDoReportsRedirectTarget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>home</body></html>")
	})
	mux.HandleFunc("/api/thing", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/uas/login", http.StatusFound)
	})
	mux.HandleFunc("/uas/login", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "sign in")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)
	resp, err := b.Transport().Do(context.Background(), &voyager.Request{
		Method: "GET", URL: srv.URL + "/api/thing",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.HasSuffix(resp.FinalURL, "/uas/login") {
		t.Errorf("FinalURL = %q, want the redirect target", resp.FinalURL)
	}
}

// TestCSRFTokenReadsPageCookie confirms the csrf token is taken from the
// page's own JSESSIONID, quotes stripped, the way LinkedIn's web app does it.
func TestCSRFTokenReadsPageCookie(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: `"ajax:99"`, Path: "/"})
		io.WriteString(w, "<html><body>home</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)
	tok, err := b.CSRFToken(context.Background())
	if err != nil {
		t.Fatalf("CSRFToken: %v", err)
	}
	if tok != "ajax:99" {
		t.Errorf("CSRFToken = %q, want %q (quotes stripped)", tok, "ajax:99")
	}
}

// TestTransportDoRejectsUnreachableHost checks a network failure surfaces as
// a Go error rather than a phantom success: fetchJS returns the failure as
// data, and Do must turn that back into an error.
func TestTransportDoRejectsUnreachableHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>home</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)
	// Port 1 on loopback: nothing listens, and the connection is refused
	// rather than hanging.
	if _, err := b.Transport().Do(context.Background(), &voyager.Request{
		Method: "GET", URL: "http://127.0.0.1:1/nope",
	}); err == nil {
		t.Fatal("Do succeeded against an unreachable host, want an error")
	}
}

// TestProfileDirRejectsTraversal keeps an alias from placing — and later
// deleting — a profile outside the lion home directory.
func TestProfileDirRejectsTraversal(t *testing.T) {
	t.Setenv("LION_HOME", t.TempDir())
	for _, bad := range []string{"../escape", "a/b", "..", ".", "/abs"} {
		if _, err := ProfileDir(bad); err == nil {
			t.Errorf("ProfileDir(%q) succeeded, want rejection", bad)
		}
	}
	dir, err := ProfileDir("work")
	if err != nil {
		t.Fatalf("ProfileDir(work): %v", err)
	}
	if filepath.Base(dir) != "work" {
		t.Errorf("ProfileDir(work) = %q", dir)
	}
}

// TestListProfilesSortedAndEmpty covers both shapes auth status depends on.
func TestListProfilesSortedAndEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LION_HOME", home)

	got, err := ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles on a fresh home: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListProfiles = %v, want none before any login", got)
	}

	for _, a := range []string{"work", "default", "alt"} {
		if _, err := ProfileDir(a); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file alongside the profile directories must not become an alias.
	if err := os.WriteFile(filepath.Join(home, "profiles", "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alt", "default", "work"}
	if len(got) != len(want) {
		t.Fatalf("ListProfiles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListProfiles = %v, want %v", got, want)
		}
	}
}

// TestSignedInRequiresSessionCookie is the regression test for a login that
// reported success while storing nothing.
//
// The original check asked only whether the URL looked logged *out* — not
// /login, /authwall, or /checkpoint/ — which passes for about:blank during
// an in-flight navigation and for LinkedIn's logged-out homepage. `auth
// login` therefore announced "signed in" the moment the window opened, and
// the profile it saved held only anonymous cookies. A session is the li_at
// cookie, so that is what gets asserted.
func TestSignedInRequiresSessionCookie(t *testing.T) {
	mux := http.NewServeMux()
	// A perfectly ordinary URL that names none of the logged-out paths, and
	// sets no session cookie — exactly the shape that used to pass.
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>marketing homepage</body></html>")
	})
	mux.HandleFunc("/authed", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "li_at", Value: "session-token", Path: "/"})
		io.WriteString(w, "<html><body>feed</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)
	if b.signedIn() {
		t.Error("signedIn() = true on a page with no li_at cookie")
	}

	if err := b.page.Navigate(srv.URL + "/authed"); err != nil {
		t.Fatal(err)
	}
	if err := b.page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	if !b.signedIn() {
		t.Error("signedIn() = false after li_at was set")
	}
}

// TestOpenReportsLoggedOutWithoutSessionCookie covers the same failure from
// the caller's side: every command routes through Open, and a profile with
// no session must be reported as logged out rather than used.
func TestOpenReportsLoggedOutWithoutSessionCookie(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>marketing homepage</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)
	if err := b.Open(context.Background()); !errors.Is(err, ErrLoggedOut) {
		t.Errorf("Open() on a session-less profile = %v, want ErrLoggedOut", err)
	}
}

// TestLoginWaitsRatherThanFalselySucceeding pins the behaviour that broke:
// with no sign-in ever completing, Login must keep waiting until its context
// expires, not return nil.
func TestLoginWaitsRatherThanFalselySucceeding(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/home", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>home</body></html>")
	})
	// Served at a path naming none of the logged-out markers, which is what
	// made this fail in the wild: LinkedIn moved the window to a URL the old
	// check had not enumerated, so "not obviously logged out" was read as
	// "signed in" and login returned success within a second.
	mux.HandleFunc("/signin", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>sign in</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBrowser(t, srv.URL)

	origLogin := loginURL
	loginURL = srv.URL + "/signin"
	defer func() { loginURL = origLogin }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := b.Login(ctx, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Login() with no sign-in = %v, want it to keep waiting until the deadline", err)
	}
}
