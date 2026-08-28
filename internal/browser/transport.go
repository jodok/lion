package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jodok/lion/internal/voyager"
)

// fetchJS issues one request from inside the loaded LinkedIn page and hands
// the result back as a plain object.
//
// Everything about this call is the browser's own: the connection and its
// TLS handshake, the User-Agent, the client hints, the fetch-metadata
// headers, their ordering, the cookie jar, and the Origin and Referer that
// come from running on a real linkedin.com document. Only the headers
// Voyager itself requires (csrf-token, the Rest.li version, x-li-track) are
// supplied, which is exactly the set the web app adds to its own XHRs.
//
// Errors are returned rather than thrown so a network failure arrives as
// data instead of a rejected promise, keeping the Go side's error handling
// in one place.
const fetchJS = `async (method, url, headersJSON, body) => {
	const init = {
		method: method,
		headers: JSON.parse(headersJSON),
		credentials: 'include',
		redirect: 'follow',
	};
	if (body) { init.body = body; }
	try {
		const res = await fetch(url, init);
		const text = await res.text();
		const headers = {};
		res.headers.forEach((v, k) => { headers[k] = v; });
		return { ok: true, status: res.status, url: res.url, body: text, headers: headers };
	} catch (e) {
		return { ok: false, error: String(e) };
	}
}`

// fetchResult mirrors what fetchJS returns.
type fetchResult struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error"`
	Status  int               `json:"status"`
	URL     string            `json:"url"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// transport implements voyager.Transport against a live browser page.
type transport struct {
	b *Browser
}

// Transport exposes the browser as a voyager.Transport. The returned value
// is only usable while the Browser is open.
func (b *Browser) Transport() voyager.Transport { return &transport{b: b} }

// Do implements voyager.Transport.
//
// The response carries no Set-Cookie header, and cannot: the Fetch standard
// makes it a forbidden response header, so page script never sees it. That
// costs less than it appears to. Cookie rotation is applied by the browser
// itself, to the profile, which is the whole reason this transport exists —
// there is no snapshot to keep in sync and nothing for lion to persist. The
// one caller that reads Set-Cookie is voyager's classifyRedirect, checking
// for LinkedIn deleting li_at outright; its other signal, the final URL
// after redirects, is reported here and still catches a session that expired
// mid-request, because such a request lands on the login wall.
//
// For the same reason this deliberately does not implement
// voyager.CookieSnapshotter. A partial snapshot would invite the CLI to
// write browser-owned cookies back into the credential store and create a
// second, diverging copy of the session.
func (t *transport) Do(ctx context.Context, req *voyager.Request) (*voyager.Response, error) {
	headers := req.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	// The browser sets User-Agent itself, and a page cannot override it
	// through fetch() — it is a forbidden request header, so a supplied one
	// is dropped silently. Removing it here keeps lion's synthesized value
	// from looking like it was meant to apply.
	sanitized := make(map[string]string, len(headers))
	for k, v := range headers {
		if http.CanonicalHeaderKey(k) == "User-Agent" {
			continue
		}
		sanitized[k] = v
	}
	headerJSON, err := json.Marshal(sanitized)
	if err != nil {
		return nil, fmt.Errorf("encode headers: %w", err)
	}

	obj, err := t.b.page.Context(ctx).Eval(fetchJS, req.Method, req.URL, string(headerJSON), string(req.Body))
	if err != nil {
		return nil, fmt.Errorf("browser transport: %w", err)
	}

	var res fetchResult
	if err := json.Unmarshal([]byte(obj.Value.JSON("", "")), &res); err != nil {
		return nil, fmt.Errorf("browser transport: decode result: %w", err)
	}
	if !res.OK {
		return nil, fmt.Errorf("browser transport: %s", res.Error)
	}

	h := make(http.Header, len(res.Headers))
	for k, v := range res.Headers {
		h.Set(k, v)
	}
	final := res.URL
	if final == "" {
		final = req.URL
	}
	return &voyager.Response{
		StatusCode: res.Status,
		Body:       []byte(res.Body),
		Headers:    h,
		FinalURL:   final,
	}, nil
}
