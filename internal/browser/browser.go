// Package browser runs LinkedIn inside a real Chromium instance that lion
// owns, and exposes it as a voyager.Transport.
//
// Why this exists: lion's other transports synthesize a browser. They send a
// hand-built User-Agent over a TLS handshake borrowed from uTLS, with none of
// the client-hint or fetch-metadata headers Chrome actually emits and no
// header ordering. LinkedIn's bot management cross-checks those signals
// against each other, and a session used that way is not merely rejected —
// it is revoked account-wide, logging the person out of their own browser
// (DESIGN.md §3.3).
//
// Driving a real browser removes the mismatch rather than narrowing it: the
// handshake, the header set, their order, and the origin are Chrome's own,
// because they are Chrome's. Requests go out as same-origin fetch() calls
// from inside a loaded linkedin.com page — the same thing the web app does
// when you click something.
//
// The session lives in a persistent Chromium profile per account alias, not
// in lion's credential store, so an unattended `lion sync` on a timer picks
// up where the last run left off exactly as a browser would. Nothing is
// pasted, and there is no cookie snapshot to go stale.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/jodok/lion/internal/config"
)

// ErrLoggedOut reports that the profile has no usable LinkedIn session: the
// browser landed on the login wall or a checkpoint instead of the feed. It
// is distinct from a transport failure because the remedy is different and
// specific — the person has to sign in themselves, in a window, once.
var ErrLoggedOut = errors.New("browser profile is not signed in to LinkedIn; run `lion auth login`")

// homeURL is the page every session loads before issuing any API call.
//
// Requests are same-origin fetch() calls, so they need a document on
// linkedin.com to run in — but the choice of document is not merely
// mechanical. The feed is what a browser loads when a person opens LinkedIn,
// and its request pattern is the one LinkedIn expects to precede API
// traffic. Firing API calls from a bare or unrelated document would be a
// behavioral tell of exactly the kind this package exists to avoid.
// It is a var rather than a const only so tests can point a session at a
// local server; nothing outside this package can reach it.
var homeURL = "https://www.linkedin.com/feed/"

// loginURL is where `auth login` sends the person to sign in themselves.
// A var, like homeURL, so tests can point sign-in at a local server.
var loginURL = "https://www.linkedin.com/login"

// Options configures a browser session.
type Options struct {
	// Alias selects the profile directory. Empty means "default".
	Alias string
	// Headed shows the browser window. Login always forces this on (a person
	// cannot sign in to a window they cannot see); ordinary commands default
	// to headless so they can run from a timer.
	Headed bool
	// ChromePath overrides the Chromium binary. Empty means: use the
	// system's Chrome if one is installed, otherwise fetch a pinned build.
	ChromePath string
	// Timeout bounds browser startup and page load.
	Timeout time.Duration
	// Verbose dumps the cookie jar to stderr at each step. Names, domains,
	// and expiries only — never values.
	Verbose bool
}

// defaultTimeout bounds launch plus the initial page load. Generous compared
// with an HTTP request's budget because it covers process startup and a full
// page render, both of which are slow on a cold profile.
const defaultTimeout = 90 * time.Second

// Browser is a running Chromium bound to one profile, with a loaded
// linkedin.com page to issue requests from. Close it when done.
type Browser struct {
	browser *rod.Browser
	page    *rod.Page
	cleanup func()
	verbose bool
}

// ProfileDir returns the Chromium user-data directory backing alias.
//
// One directory per alias keeps lion's existing multi-account model intact
// while moving the actual session state into the browser: switching accounts
// is switching profiles, and `auth logout` is deleting one, which revokes
// exactly as much as it should and nothing more.
func ProfileDir(alias string) (string, error) {
	if alias == "" {
		alias = "default"
	}
	// Aliases reach this from --account and from the credential store, so a
	// value containing a path separator or traversal would otherwise place a
	// browser profile — and later delete it — anywhere on disk.
	if alias != filepath.Base(alias) || alias == "." || alias == ".." {
		return "", fmt.Errorf("invalid account alias %q", alias)
	}
	home, err := config.EnsureHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "profiles", alias)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Launch starts Chromium against alias's profile and loads LinkedIn.
//
// It returns ErrLoggedOut when the profile has no live session, so callers
// can tell "you need to sign in" apart from "the browser would not start" —
// the first is routine and actionable, the second is not.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// The browser outlives this call, so its context must be cancelable but
	// not deadlined: a timeout here would bound the whole session and kill
	// Chromium mid-command on any run longer than the startup budget (a
	// paged sync, a long export). Startup gets the deadline instead, via
	// startCtx below, and each request carries its own from the caller.
	ctx, cancel := context.WithCancel(ctx)
	startCtx, cancelStart := context.WithTimeout(ctx, timeout)
	defer cancelStart()

	dir, err := ProfileDir(opts.Alias)
	if err != nil {
		cancel()
		return nil, err
	}

	bin, err := resolveChrome(opts.ChromePath)
	if err != nil {
		cancel()
		return nil, err
	}

	l := launcher.New().
		Bin(bin).
		UserDataDir(dir).
		// Chromium refuses to reuse a profile another process holds, which
		// is what a second concurrent lion command would be. Leakless off
		// keeps the child from outliving a killed parent on macOS.
		Leakless(true).
		Headless(!opts.Headed)
	// rod sets enable-automation unconditionally, which is what turns on
	// navigator.webdriver and Chrome's "controlled by automated test
	// software" state. Removing it is squarely the point of this package.
	l = l.Delete("enable-automation")
	if !opts.Headed {
		// navigator.webdriver and the AutomationControlled blink feature are
		// the two signals that separate a CDP-driven Chrome from a hand-driven
		// one in ordinary page script. Neither touches the TLS handshake or
		// the header set, which is where the previous transport actually gave
		// itself away, but both are cheap to remove and there is no reason to
		// leave a tell in place.
		l = l.Set("disable-blink-features", "AutomationControlled")
	}

	wsURL, err := l.Context(startCtx).Launch()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("launch chromium: %w", err)
	}

	br := rod.New().ControlURL(wsURL).Context(ctx)
	if err := br.Connect(); err != nil {
		cancel()
		l.Kill()
		return nil, fmt.Errorf("connect to chromium: %w", err)
	}

	b := &Browser{
		browser: br,
		verbose: opts.Verbose,
		cleanup: func() {
			// Ask Chromium to shut down and give it time to finish before
			// resorting to force. This ordering is load-bearing: Chromium
			// holds its cookie store in memory and commits it on a timer and
			// at clean shutdown, while launcher.Kill sends SIGKILL after a
			// one-second sleep. Killing straight after Close truncated every
			// cookie set since the last commit — which is to say the entire
			// session, since signing in is the last thing that happens before
			// the browser is closed. The profile kept the anonymous cookies
			// from the first page load and lost li_at.
			_ = br.Close()
			if !waitForExit(l.PID(), browserExitGrace) {
				l.Kill() // ignored the close request; force it
			}
			cancel()
		},
	}

	page, err := br.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("open page: %w", err)
	}
	if !opts.Headed {
		// Headless Chrome advertises "HeadlessChrome/<version>" in its UA
		// while presenting headed Chrome's TLS fingerprint — the same
		// self-contradiction that got the previous transport revoked, just
		// one layer up. Strip the marker so the two agree.
		ua, err := page.Eval(`() => navigator.userAgent`)
		if err == nil {
			fixed := strings.Replace(ua.Value.Str(), "HeadlessChrome/", "Chrome/", 1)
			_ = proto.NetworkSetUserAgentOverride{UserAgent: fixed}.Call(page)
		}
	}
	b.page = page
	// Inject before the caller navigates: cookies have to be in place for
	// the very first request, which is the one that decides whether LinkedIn
	// serves the feed or the login wall.
	if err := b.RestoreSession(ctx, opts.Alias); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// Open loads LinkedIn and reports whether the profile is signed in.
//
// Separate from Launch so `auth login` can start a browser that is expected
// to be logged out and drive the sign-in itself, rather than having Launch
// fail on the very condition login exists to fix.
func (b *Browser) Open(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	if err := b.page.Context(ctx).Navigate(homeURL); err != nil {
		return fmt.Errorf("load linkedin: %w", err)
	}
	if err := b.page.Context(ctx).WaitLoad(); err != nil {
		return fmt.Errorf("load linkedin: %w", err)
	}
	// Dumped before the verdict, so a --verbose run shows what the jar
	// actually held when the decision was made rather than only that it
	// went badly.
	b.DumpCookies("after loading linkedin")
	ok, err := b.signedIn()
	if err != nil {
		return fmt.Errorf("check session: %w", err)
	}
	if !ok {
		return ErrLoggedOut
	}
	return nil
}

// sessionCookieName is the cookie that *is* a LinkedIn session. Its presence
// is the only positive, unambiguous evidence that authentication completed.
const sessionCookieName = "li_at"

// hasSession reports whether the profile holds a LinkedIn session cookie.
//
// li_at is httpOnly, so this goes through CDP rather than document.cookie.
func (b *Browser) hasSession() (bool, error) {
	// Empty list means "cookies for the page we are actually on", which
	// keeps this correct without hardcoding an origin the tests override.
	cookies, err := b.page.Cookies(nil)
	if err != nil {
		return false, err
	}
	for _, c := range cookies {
		if c.Name == sessionCookieName && c.Value != "" {
			return true, nil
		}
	}
	return false, nil
}

// signedIn reports whether this profile has an authenticated LinkedIn
// session.
//
// The load-bearing half is hasSession: a session is the li_at cookie, so
// asking whether it exists is a positive test that cannot be satisfied by
// accident. An earlier version asked only whether the current URL looked
// logged *out* — no /login, /authwall, or /checkpoint/ — which inverted the
// burden of proof and failed open on everything it had not enumerated.
// about:blank during an in-flight navigation and LinkedIn's logged-out
// marketing homepage both passed, so `auth login` reported success the
// instant the window opened and stored a profile holding nothing but
// anonymous cookies.
//
// The URL check is kept as a second condition rather than replaced, because
// the two catch different things: a checkpoint is reachable while li_at
// still exists, and that is a session lion cannot use yet.
func (b *Browser) signedIn() (bool, error) {
	ok, err := b.hasSession()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	info, err := b.page.Info()
	if err != nil {
		return false, err
	}
	u := strings.ToLower(info.URL)
	switch {
	case strings.Contains(u, "/uas/login"), strings.Contains(u, "/login"):
		return false, nil
	case strings.Contains(u, "/authwall"):
		return false, nil
	case strings.Contains(u, "/checkpoint/"):
		return false, nil
	}
	return true, nil
}

// Login drives an interactive sign-in: it opens the login page and waits
// until LinkedIn lands the person on an authenticated URL.
//
// lion deliberately does not type credentials, answer two-factor prompts, or
// touch a challenge. Those are the person's to complete in the window, which
// is both the only honest way to handle someone's password and the only way
// a checkpoint is meant to be satisfied. All this does is hold the browser
// open until the session exists, then let the profile persist it.
func (b *Browser) Login(ctx context.Context, poll time.Duration) error {
	if poll <= 0 {
		poll = time.Second
	}
	if err := b.page.Context(ctx).Navigate(loginURL); err != nil {
		return fmt.Errorf("open login page: %w", err)
	}
	// A browser that has gone away answers every probe with an error, which
	// is indistinguishable from "not signed in yet" if only the bool is
	// consulted — so closing the sign-in window used to leave lion polling
	// silently until the ten-minute deadline and then blaming the person for
	// being slow. Consecutive failures mean the browser is gone; a single one
	// can just be a navigation swapping the execution context out underfoot,
	// which happens constantly during a real sign-in.
	const goneAfter = 3
	var consecutive int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		ok, err := b.signedIn()
		if err != nil {
			consecutive++
			if consecutive >= goneAfter {
				return fmt.Errorf("browser closed before sign-in completed: %w", err)
			}
			continue
		}
		consecutive = 0
		if ok {
			return nil
		}
	}
}

// browserExitGrace bounds how long Close waits for Chromium to exit on its
// own after being asked to. Long enough to commit a cookie store, short
// enough that a wedged browser does not hang a CLI command.
const browserExitGrace = 10 * time.Second

// sessionRetention is the expiry given to a session-scoped li_at when it is
// promoted to a persistent cookie. LinkedIn issues roughly a year when "keep
// me logged in" applies, so this matches what the same sign-in would have
// produced with that box ticked.
const sessionRetention = 365 * 24 * time.Hour

// waitForExit blocks until pid is gone or the grace period elapses, and
// reports whether it observed the exit. Signal 0 performs the liveness check
// without delivering anything.
//
// The bool matters: rod's launcher.Kill sleeps a full second before
// signalling, so calling it after the browser has already exited makes every
// command a second slower and aims a kill at a PID the OS may since have
// handed to something else.
func waitForExit(pid int, grace time.Duration) bool {
	if pid == 0 {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true // exited
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// PersistSession makes the session survive the browser closing.
//
// LinkedIn issues li_at as a session cookie unless "keep me logged in"
// applied, and Chromium holds session cookies in memory only — so a profile
// that is signed in right now can come back empty on the next run, through
// no fault of the shutdown path. Since lion's whole reason for owning a
// profile is that an unattended run picks up where the last one left off,
// re-set such a cookie with an explicit expiry, which is precisely what
// ticking that box would have done.
//
// A cookie LinkedIn already made persistent is left exactly as issued.
func (b *Browser) PersistSession(ctx context.Context) (bool, error) {
	cookies, err := b.page.Context(ctx).Cookies(nil)
	if err != nil {
		return false, err
	}
	for _, c := range cookies {
		if c.Name != sessionCookieName || c.Value == "" {
			continue
		}
		if c.Expires > 0 {
			return false, nil // already persistent; leave it alone
		}
		exp := proto.TimeSinceEpoch(float64(time.Now().Add(sessionRetention).Unix()))
		_, err := proto.NetworkSetCookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
			SameSite: c.SameSite,
			Expires:  exp,
		}.Call(b.page)
		if err != nil {
			return false, fmt.Errorf("persist session cookie: %w", err)
		}
		return true, nil
	}
	return false, ErrLoggedOut
}

// DumpCookies writes the live jar to stderr under label when Verbose is set.
// Deliberately names, domains, and expiries only: the values are the session
// itself, and a diagnostic that prints them turns a support paste into a
// credential leak.
func (b *Browser) DumpCookies(label string) {
	if !b.verbose {
		return
	}
	cookies, err := b.page.Cookies(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[cookies %s] error: %v\n", label, err)
		return
	}
	page := "?"
	if info, err := b.page.Info(); err == nil {
		page = info.URL
	}
	fmt.Fprintf(os.Stderr, "[cookies %s] page=%s count=%d\n", label, page, len(cookies))
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		exp := "session"
		if c.Expires > 0 {
			exp = time.Unix(int64(c.Expires), 0).UTC().Format(time.RFC3339)
		}
		names = append(names, fmt.Sprintf("    %-14s domain=%-22s path=%-4s secure=%v httpOnly=%v sameSite=%-6s expires=%s",
			c.Name, c.Domain, c.Path, c.Secure, c.HTTPOnly, c.SameSite, exp))
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(os.Stderr, n)
	}
}

// Close shuts the browser down and releases the profile lock. Safe to call
// more than once.
func (b *Browser) Close() {
	if b.cleanup != nil {
		b.cleanup()
		b.cleanup = nil
	}
}

// resolveChrome picks the Chromium binary: an explicit override, then a
// system Chrome, then a pinned build fetched into rod's cache.
//
// Preferring an installed Chrome is not just about saving a download. A
// stock Chrome is the exact binary whose fingerprint LinkedIn sees from
// millions of ordinary users, and it stays current on its own; a pinned
// download drifts out of date the moment Chrome ships a release, which is
// the failure mode this whole package is replacing. The download is the
// fallback so that an unattended host with no browser still works.
func resolveChrome(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("chrome path %q: %w", override, err)
		}
		return override, nil
	}
	if path, ok := launcher.LookPath(); ok {
		return path, nil
	}
	path, err := launcher.NewBrowser().Get()
	if err != nil {
		return "", fmt.Errorf("no chrome found and download failed: %w", err)
	}
	return path, nil
}

// CSRFToken returns the value Voyager expects in the csrf-token header.
//
// LinkedIn derives it from the JSESSIONID cookie with the surrounding quotes
// stripped, and its own web app reads that cookie from document.cookie to
// build the header — JSESSIONID is deliberately not httpOnly for exactly
// this reason. Reading it the same way keeps lion from needing a copy of the
// session outside the browser profile.
func (b *Browser) CSRFToken(ctx context.Context) (string, error) {
	obj, err := b.page.Context(ctx).Eval(`() => {
		const m = document.cookie.match(/(?:^|;\s*)JSESSIONID=([^;]*)/);
		return m ? m[1] : '';
	}`)
	if err != nil {
		return "", fmt.Errorf("read csrf token: %w", err)
	}
	tok := strings.Trim(obj.Value.Str(), `"`)
	if tok == "" {
		return "", ErrLoggedOut
	}
	return tok, nil
}

// DeleteProfile removes alias's browser profile, ending that session.
//
// This is what `auth logout` means once the session lives in the browser:
// the cookies are in the profile, so deleting the directory is what actually
// revokes local access. Leaving it behind would keep a working, signed-in
// LinkedIn session on disk after the person asked lion to forget it.
func DeleteProfile(alias string) error {
	dir, err := ProfileDir(alias)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// ListProfiles returns the aliases that have a browser profile on disk,
// sorted so `auth status` output is stable rather than filesystem-ordered.
func ListProfiles() ([]string, error) {
	home, err := config.EnsureHome()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(home, "profiles"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
