package voyager

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// chromeClientTimeoutSeconds bounds the whole request lifecycle (connect,
// TLS handshake, redirects, body read), mirroring stdlibTransport's timeout.
const chromeClientTimeoutSeconds = 30

// linkedInCookieURL scopes the seeded cookie jar. Cookies are set with
// Domain ".linkedin.com" (see cookiesToFHTTP), so any linkedin.com host —
// including redirect targets — receives them; this URL just anchors the
// jar's initial SetCookies call.
var linkedInCookieURL = &url.URL{Scheme: "https", Host: "www.linkedin.com", Path: "/"}

// chromeTransport is the production Transport: it impersonates a real
// Chrome TLS handshake via github.com/bogdanfinn/tls-client (uTLS) so
// requests pass LinkedIn's Cloudflare bot-management fingerprinting, which
// blocks stdlib net/http's detectably-not-Chrome ClientHello. See
// DESIGN.md §3.3.
type chromeTransport struct {
	http tlsclient.HttpClient
}

// NewChromeTransport builds a Transport that impersonates Chrome's TLS
// fingerprint, seeded with cookies — ideally the full browser cookie jar
// (li_at, JSESSIONID, bcookie, bscookie, lidc, li_gc, ...), since GraphQL
// endpoints reject a bare li_at+JSESSIONID pair.
//
// GOTCHA: tls-client's cookie jar silently ignores a manually-set Cookie
// request header. Cookies must be seeded into the jar itself via
// jar.SetCookies, which is what this constructor does; Do (below) never
// sets a Cookie header.
func NewChromeTransport(cookies map[string]string) (Transport, error) {
	jar := tlsclient.NewCookieJar()
	jar.SetCookies(linkedInCookieURL, cookiesToFHTTP(cookies))

	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithTimeoutSeconds(chromeClientTimeoutSeconds),
		tlsclient.WithClientProfile(profiles.Chrome_146),
		tlsclient.WithCookieJar(jar),
		// Redirects are followed (tls-client's default) — LinkedIn hands out
		// lidc (data-center routing) and Cloudflare __cf_bm via redirects
		// that the session needs to look authentic.
	)
	if err != nil {
		return nil, fmt.Errorf("chrome transport: %w", err)
	}
	return &chromeTransport{http: client}, nil
}

// cookiesToFHTTP converts lion's cookie map into fhttp cookies scoped to
// the whole linkedin.com domain. Values (notably JSESSIONID) are used
// verbatim, including any surrounding quotes LinkedIn expects; Quoted is
// left false so the wire encoder doesn't add a second pair of quotes on
// top of ones already embedded in the value.
func cookiesToFHTTP(cookies map[string]string) []*fhttp.Cookie {
	out := make([]*fhttp.Cookie, 0, len(cookies))
	for name, value := range cookies {
		out = append(out, &fhttp.Cookie{
			Name:   name,
			Value:  value,
			Domain: ".linkedin.com",
			Path:   "/",
		})
	}
	return out
}

// Snapshot implements voyager.CookieSnapshotter: it returns the transport's
// live cookie jar, including any values LinkedIn rotated in via Set-Cookie
// during this process's requests (JSESSIONID, li_at, and lidc rotate
// continuously; the Cloudflare __cf_bm cookie expires — DESIGN.md §3.3), so
// the CLI can persist them for the next invocation instead of reloading the
// stale snapshot `auth login` stored.
func (t *chromeTransport) Snapshot() map[string]string {
	cookies := t.http.GetCookies(linkedInCookieURL)
	out := make(map[string]string, len(cookies))
	for _, c := range cookies {
		out[c.Name] = cookieWireValue(c.Value, c.Quoted)
	}
	return out
}

// Do implements Transport by converting req into an fhttp request, sending
// it through the Chrome-impersonating client, and converting the response
// back into lion's transport-agnostic Response.
func (t *chromeTransport) Do(ctx context.Context, req *Request) (*Response, error) {
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	hr, err := fhttp.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hr.Header.Set(k, v)
	}
	resp, err := t.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("chrome transport: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Body:       b,
		Headers:    cloneFHTTPHeader(resp.Header),
		FinalURL:   fhttpFinalURL(resp.Request, req.URL),
	}, nil
}

// cloneFHTTPHeader converts fhttp's response header map into the stdlib
// http.Header the rest of lion works with; both are map[string][]string
// under an fhttp/net-http-specific named type, so this is a plain copy.
func cloneFHTTPHeader(h fhttp.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// fhttpFinalURL mirrors transport.go's finalURL for fhttp's request/response
// types: tls-client follows redirects (see NewChromeTransport) and, like
// net/http, sets Response.Request to the last request actually sent, so its
// URL reflects any redirect target.
func fhttpFinalURL(lastReq *fhttp.Request, requested string) string {
	if lastReq != nil && lastReq.URL != nil {
		return lastReq.URL.String()
	}
	return requested
}
