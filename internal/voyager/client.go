// Package voyager is a client for LinkedIn's internal "Voyager" API — the same
// private API the linkedin.com web app uses. It authenticates with browser
// session cookies (li_at + JSESSIONID) and speaks both the older REST-li
// endpoints and the newer GraphQL surface.
//
// This package never imports cobra or writes to stdout; it returns typed
// domain values so the CLI layer stays thin and the client stays testable with
// recorded fixtures (transport is injectable).
package voyager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jodok/lion/internal/ratelimit"
)

const (
	baseURL    = "https://www.linkedin.com/voyager/api"
	restliVer  = "2.0.0"
	acceptType = "application/vnd.linkedin.normalized+json+2.1"
	// A realistic desktop UA reduces the chance of being flagged.
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
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
	http      *http.Client
	csrf      string
	liAt      string
	jsession  string
	limiter   *ratelimit.Limiter
	dryRun    bool
	baseURL   string
	userAgent string
}

// Option configures a Client.
type Option func(*Client)

// WithTransport injects an http.RoundTripper (used by tests to replay
// fixtures).
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) { c.http.Transport = rt }
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

// New builds a Client from session cookies. jsessionID is used both as a
// cookie and (with quotes stripped) as the csrf-token header.
func New(liAt, jsessionID string, opts ...Option) *Client {
	c := &Client{
		http:      &http.Client{Timeout: 30 * time.Second},
		csrf:      strings.Trim(jsessionID, `"`),
		liAt:      liAt,
		jsession:  jsessionID,
		limiter:   ratelimit.New(ratelimit.DefaultBudgets()),
		baseURL:   baseURL,
		userAgent: userAgent,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// DryRun reports whether mutations are suppressed.
func (c *Client) DryRun() bool { return c.dryRun }

// get performs a rate-limited GET and returns the response body.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if err := c.limiter.Wait(ctx, ratelimit.Read); err != nil {
		return nil, err
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	return c.do(req)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	return c.do(req)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Csrf-Token", c.csrf)
	req.Header.Set("X-RestLi-Protocol-Version", restliVer)
	req.Header.Set("X-Li-Lang", "en_US")
	req.Header.Set("Accept", acceptType)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cookie", fmt.Sprintf("li_at=%s; JSESSIONID=%s", c.liAt, c.jsession))
}

// do executes a request with one retry on 429/5xx and maps errors to sentinels.
func (c *Client) do(req *http.Request) ([]byte, error) {
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return body, nil
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, ErrUnauthorized
		case resp.StatusCode == http.StatusForbidden:
			// LinkedIn returns 403 for both expired CSRF and checkpoints.
			if strings.Contains(strings.ToLower(string(body)), "challenge") {
				return nil, ErrChallenge
			}
			return nil, ErrUnauthorized
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, ErrRateLimited
		case resp.StatusCode >= 500 && attempt < maxAttempts:
			lastErr = &APIError{Status: resp.StatusCode, Body: string(body)}
			continue
		default:
			return nil, &APIError{Status: resp.StatusCode, Body: string(body)}
		}
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
