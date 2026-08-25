package voyager

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strings"
	"time"
)

// Transport sends one HTTP request and returns the response. It is the seam that
// lets lion swap how bytes reach LinkedIn without touching any client logic.
//
// Why this exists: LinkedIn fronts Voyager with Cloudflare bot management that
// fingerprints the TLS handshake. Go's stdlib net/http handshake is detectably
// not-Chrome and gets blocked on the GraphQL endpoints (302 + session wipe). The
// production transport therefore impersonates Chrome's TLS fingerprint (uTLS),
// while tests inject a fixture transport. See DESIGN.md §3.3.
//
// A Transport owns its cookie jar so it can accumulate server-set cookies
// (lidc data-center routing, Cloudflare __cf_bm) across redirects. The Client
// supplies the authenticated session cookies once, at construction.
type Transport interface {
	Do(ctx context.Context, req *Request) (*Response, error)
}

// CookieSnapshotter is an optional Transport capability: a transport that
// owns a cookie jar can return its current cookies, including values
// LinkedIn rotated in via Set-Cookie during the process's lifetime, so the
// CLI can persist them for the next invocation rather than reloading the
// stale snapshot `auth login` stored (DESIGN.md §3.3 — JSESSIONID, li_at,
// and lidc rotate continuously, and the Cloudflare __cf_bm cookie expires).
type CookieSnapshotter interface {
	Snapshot() map[string]string
}

// Request is a transport-agnostic HTTP request. Body is a byte slice (nil for
// GET) so a retry can resend it without re-buffering.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// Response is a transport-agnostic HTTP response. The body is fully read; the
// status is enough for the Client to map most errors to exit-code sentinels.
// Headers and FinalURL exist for the harder case: a session-expired redirect
// to a login/checkpoint page, which arrives as a plain 2xx (see
// client.go's classifyRedirect and DESIGN.md §3.3).
type Response struct {
	StatusCode int
	Body       []byte
	// Headers are the final response's headers (after following redirects).
	Headers http.Header
	// FinalURL is the URL the response actually came from, after any
	// redirects were followed. It differs from the request URL only when a
	// redirect occurred.
	FinalURL string
}

// cookieHeader renders a cookie map as a single Cookie header value, in a stable
// order. Values are used verbatim (JSESSIONID is stored with its surrounding
// quotes, which LinkedIn expects).
func cookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(cookies))
	for k := range cookies {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(cookies[k])
	}
	return b.String()
}

// stdlibTransport is the default Transport, backed by net/http with a cookie
// jar. It works for endpoints not behind Cloudflare's TLS fingerprinting (e.g.
// /me); the Chrome-impersonating transport is required for the rest and is wired
// in separately.
type stdlibTransport struct {
	http    *http.Client
	cookies map[string]string
}

func newStdlibTransport(cookies map[string]string) *stdlibTransport {
	jar, _ := cookiejar.New(nil)
	return &stdlibTransport{
		http:    &http.Client{Timeout: 30 * time.Second, Jar: jar},
		cookies: cookies,
	}
}

// Snapshot implements CookieSnapshotter for stdlibTransport, reading back
// whatever the jar currently holds for linkedin.com — including any cookies
// the server rotated in via Set-Cookie during this process's requests.
func (t *stdlibTransport) Snapshot() map[string]string {
	if t.http.Jar == nil {
		return nil
	}
	return cookiesToMap(t.http.Jar.Cookies(linkedInCookieURL))
}

// cookiesToMap converts a cookie jar's cookies into lion's name->value map.
// net/http's (and fhttp's, used by chrome_transport.go) cookie parser strips
// a value's surrounding quotes into the Quoted flag rather than leaving them
// in Value, so this reconstructs the wire form via cookieWireValue —
// otherwise a snapshot would silently drop JSESSIONID's required quoting.
func cookiesToMap(cookies []*http.Cookie) map[string]string {
	out := make(map[string]string, len(cookies))
	for _, c := range cookies {
		out[c.Name] = cookieWireValue(c.Value, c.Quoted)
	}
	return out
}

// cookieWireValue reconstructs a cookie value exactly as it appeared on the
// wire from a parsed Cookie's (Value, Quoted) pair, shared by both
// stdlibTransport (net/http.Cookie) and chromeTransport (fhttp.Cookie) since
// both types define the same two fields.
func cookieWireValue(value string, quoted bool) string {
	if quoted {
		return `"` + value + `"`
	}
	return value
}

func (t *stdlibTransport) Do(ctx context.Context, req *Request) (*Response, error) {
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	if ch := cookieHeader(t.cookies); ch != "" {
		hr.Header.Set("Cookie", ch)
	}
	resp, err := t.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Body:       b,
		Headers:    cloneHeader(resp.Header),
		FinalURL:   finalURL(resp.Request, req.URL),
	}, nil
}

// cloneHeader copies a response header map so callers never alias the
// transport's internal state.
func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// finalURL returns the URL a response actually came from. net/http (and,
// per its documented net/http-compatible behavior, tls-client) sets
// Response.Request to the last request sent, so after following redirects
// its URL is the final one; fall back to the originally requested URL if
// that's unavailable.
func finalURL(lastReq *http.Request, requested string) string {
	if lastReq != nil && lastReq.URL != nil {
		return lastReq.URL.String()
	}
	return requested
}
