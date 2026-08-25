// Package voyager is a client for LinkedIn's internal "Voyager" API — the same
// private API the linkedin.com web app uses. It authenticates with browser
// session cookies (li_at + JSESSIONID, and ideally the full cookie jar) and
// speaks both the older REST-li endpoints and the newer GraphQL surface.
//
// This package never imports cobra or writes to stdout; it returns typed
// domain values so the CLI layer stays thin. All network I/O goes through the
// Transport seam (see transport.go), which keeps the client testable with
// recorded fixtures and lets production impersonate Chrome's TLS fingerprint.
package voyager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/jodok/lion/internal/ratelimit"
)

const (
	baseURL    = "https://www.linkedin.com/voyager/api"
	restliVer  = "2.0.0"
	acceptType = "application/vnd.linkedin.normalized+json+2.1"
	// A realistic desktop UA reduces the chance of being flagged.
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
	// liTrack identifies the client. Voyager's GraphQL gateway requires it even
	// when simple REST endpoints (e.g. /me) tolerate its absence. clientVersion
	// tracks a recent voyager-web build; it rarely needs to be exact.
	liTrack = `{"clientVersion":"1.13.36348","mpVersion":"1.13.36348",` +
		`"osName":"web","timezoneOffset":1,"deviceFormFactor":"DESKTOP",` +
		`"mpName":"voyager-web","displayDensity":2,"displayWidth":2560,"displayHeight":1440}`
)

// APIError describes a non-2xx Voyager response, carrying enough for the CLI
// to map it to a stable exit code.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("voyager: HTTP %d: %s", e.Status, truncate(e.Body, 300))
}

// Sentinel errors for CLI exit-code mapping.
var (
	ErrUnauthorized = errors.New("not authenticated (cookie expired?)")
	ErrRateLimited  = errors.New("rate limited by LinkedIn")
	ErrNotFound     = errors.New("not found")
	ErrChallenge    = errors.New("LinkedIn checkpoint/challenge required")
)

// Client talks to Voyager. Construct with New.
type Client struct {
	transport Transport
	cookies   map[string]string
	csrf      string
	limiter   *ratelimit.Limiter
	dryRun    bool
	baseURL   string
	userAgent string
}

// Option configures a Client.
type Option func(*Client)

// WithTransport injects a Transport (used by tests to replay fixtures, and by
// the CLI to install the Chrome-impersonating transport).
func WithTransport(t Transport) Option {
	return func(c *Client) { c.transport = t }
}

// WithCookies supplies the full browser cookie jar (beyond li_at + JSESSIONID).
// GraphQL endpoints want the complete set; merge it in before the default
// transport is constructed.
func WithCookies(cookies map[string]string) Option {
	return func(c *Client) {
		for k, v := range cookies {
			c.cookies[k] = v
		}
	}
}

// WithLimiter sets a custom rate limiter.
func WithLimiter(l *ratelimit.Limiter) Option {
	return func(c *Client) { c.limiter = l }
}

// WithDryRun makes mutating calls return without hitting the network.
func WithDryRun(dry bool) Option {
	return func(c *Client) { c.dryRun = dry }
}

// WithBaseURL overrides the API base (used by tests).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// New builds a Client from session cookies. jsessionID is used both as a cookie
// and (with quotes stripped) as the csrf-token header. Pass WithCookies to add
// the rest of the browser jar.
func New(liAt, jsessionID string, opts ...Option) *Client {
	c := &Client{
		cookies:   map[string]string{"li_at": liAt, "JSESSIONID": jsessionID},
		limiter:   ratelimit.NewDefault(ratelimit.DefaultBudgets()),
		baseURL:   baseURL,
		userAgent: userAgent,
	}
	for _, o := range opts {
		o(c)
	}
	// csrf-token is the JSESSIONID with surrounding quotes stripped; recompute
	// after options in case WithCookies replaced JSESSIONID.
	c.csrf = strings.Trim(c.cookies["JSESSIONID"], `"`)
	if c.transport == nil {
		c.transport = newStdlibTransport(c.cookies)
	}
	return c
}

// DryRun reports whether mutations are suppressed.
func (c *Client) DryRun() bool { return c.dryRun }

// baseHeaders returns the headers common to every Voyager request. Cookies are
// added by the transport, not here.
func (c *Client) baseHeaders() map[string]string {
	return map[string]string{
		"Csrf-Token":                c.csrf,
		"X-RestLi-Protocol-Version": restliVer,
		"X-Li-Lang":                 "en_US",
		"X-Li-Track":                liTrack,
		"Accept":                    acceptType,
		"User-Agent":                c.userAgent,
	}
}

// get performs a rate-limited GET and returns the response body.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if err := c.limiter.Wait(ctx, ratelimit.Read); err != nil {
		return nil, err
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return c.do(ctx, &Request{Method: "GET", URL: u, Headers: c.baseHeaders()})
}

// getRawQuery performs a rate-limited GET where rawQuery is used verbatim as the
// URL query string (no re-encoding). Needed for Voyager GraphQL, whose Rest.li
// variables encoding must not be mangled by url.Values.
func (c *Client) getRawQuery(ctx context.Context, path, rawQuery string) ([]byte, error) {
	if err := c.limiter.Wait(ctx, ratelimit.Read); err != nil {
		return nil, err
	}
	u := c.baseURL + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return c.do(ctx, &Request{Method: "GET", URL: u, Headers: c.baseHeaders()})
}

// post performs a rate-limited POST. class controls pacing. When dryRun is set,
// it returns (nil, nil) without sending — callers must treat a nil body under
// dry-run as "would have sent".
func (c *Client) post(ctx context.Context, path string, body io.Reader, class ratelimit.Class) ([]byte, error) {
	if c.dryRun {
		return nil, nil
	}
	if err := c.limiter.Wait(ctx, class); err != nil {
		return nil, err
	}
	var buf []byte
	if body != nil {
		var err error
		if buf, err = io.ReadAll(body); err != nil {
			return nil, err
		}
	}
	h := c.baseHeaders()
	h["Content-Type"] = "application/json; charset=UTF-8"
	return c.do(ctx, &Request{Method: "POST", URL: c.baseURL + path, Headers: h, Body: buf})
}

// do sends a request and maps errors to sentinels. The request Body is a
// byte slice, so retrying (when it happens) is safe without re-buffering.
// 429 is surfaced immediately as ErrRateLimited rather than retried.
//
// Only GET is retried on transport error / 5xx. GET is idempotent, so a
// retry after a dropped connection or a flaky 500 is safe. POST is not:
// retrying a mutation whose request reached LinkedIn but whose response
// didn't reach us could duplicate a sent message, invite, comment, or post.
// A POST therefore gets exactly one attempt; the caller sees the error and
// decides whether to retry (with its own confirmation, if appropriate).
func (c *Client) do(ctx context.Context, req *Request) ([]byte, error) {
	maxAttempts := 1
	if req.Method == "GET" {
		maxAttempts = 2
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := c.transport.Do(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		if sentinel := classifyRedirect(resp); sentinel != nil {
			return nil, sentinel
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp.Body, nil
		case resp.StatusCode == 401:
			return nil, ErrUnauthorized
		case resp.StatusCode == 403:
			// LinkedIn returns 403 for expired CSRF, checkpoints, and
			// challenges; DESIGN.md §4 exit code 7 covers both spellings.
			body := strings.ToLower(string(resp.Body))
			if strings.Contains(body, "challenge") || strings.Contains(body, "checkpoint") {
				return nil, ErrChallenge
			}
			return nil, ErrUnauthorized
		case resp.StatusCode == 404:
			return nil, ErrNotFound
		case resp.StatusCode == 429:
			return nil, ErrRateLimited
		case resp.StatusCode >= 500 && attempt < maxAttempts:
			lastErr = &APIError{Status: resp.StatusCode, Body: string(resp.Body)}
			continue
		default:
			return nil, &APIError{Status: resp.StatusCode, Body: string(resp.Body)}
		}
	}
	return nil, lastErr
}

// classifyRedirect inspects a response's final URL and Set-Cookie headers
// for the two clear signals a session-expired redirect leaves behind:
// landing on a login/checkpoint page, or LinkedIn deleting li_at outright.
// Both Transport implementations follow redirects (see transport.go,
// chrome_transport.go), so a session that expired mid-request comes back as
// a plain 200 serving login/checkpoint HTML instead of a 401/403 — without
// this check, that HTML fails JSON decoding downstream and surfaces as a
// generic error (exit 1) instead of auth (3) / challenge (7). Deliberately
// conservative: only these exact signals reclassify a response; anything
// else falls through to normal status-code handling.
func classifyRedirect(resp *Response) error {
	if resp == nil {
		return nil
	}
	final := strings.ToLower(resp.FinalURL)
	switch {
	case strings.Contains(final, "/checkpoint/"):
		return ErrChallenge
	case strings.Contains(final, "/uas/login"), strings.Contains(final, "/authwall"):
		return ErrUnauthorized
	case final != "" && !strings.Contains(final, "/voyager/api/"):
		// Redirected clean off the Voyager API surface entirely (e.g. to a
		// plain www.linkedin.com page) — not a Voyager JSON response.
		return ErrUnauthorized
	}
	for _, v := range resp.Headers.Values("Set-Cookie") {
		if strings.Contains(v, "li_at=delete") {
			return ErrUnauthorized
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
