package voyager

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jodok/lion/internal/ratelimit"
)

// sequenceTransport returns responses from a fixed sequence (by call index)
// and counts how many times Do was called, so tests can assert retry
// behavior precisely rather than inferring it from side effects.
type sequenceTransport struct {
	responses []*Response
	calls     int
}

func (s *sequenceTransport) Do(_ context.Context, _ *Request) (*Response, error) {
	i := s.calls
	s.calls++
	if i < len(s.responses) {
		return s.responses[i], nil
	}
	if len(s.responses) > 0 {
		return s.responses[len(s.responses)-1], nil
	}
	return &Response{StatusCode: 500}, nil
}

// snapshotTransport is a fixture Transport that also implements
// CookieSnapshotter, standing in for chromeTransport/stdlibTransport's live
// jar without needing a real one.
type snapshotTransport struct {
	sequenceTransport
	snapshot map[string]string
}

func (s *snapshotTransport) Snapshot() map[string]string { return s.snapshot }

// TestCookiesOverlaysSnapshotOverStatic pins the merge rule: the transport's
// live jar (a stand-in for cookies LinkedIn rotated in mid-session) wins
// per-name over the static cookies passed at construction, while a name only
// present in the static set is preserved.
func TestCookiesUsesSnapshotAsAuthoritativeSet(t *testing.T) {
	// The jar is seeded with the static cookies, so its contents are already
	// "what we started with, plus rotations, minus anything that expired".
	// bcookie is deliberately absent from the snapshot: it must NOT come back
	// from the static set, or an expired cookie would be resurrected and
	// re-seeded on every later run.
	st := &snapshotTransport{snapshot: map[string]string{
		"li_at":      "li_at_test",
		"JSESSIONID": `"rotated:1"`, // rotated mid-session
		"lidc":       "dc-2",        // picked up mid-session
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st),
		WithCookies(map[string]string{"bcookie": "static-b"}))
	got := c.Cookies()
	want := map[string]string{
		"li_at":      "li_at_test",
		"JSESSIONID": `"rotated:1"`,
		"lidc":       "dc-2",
	}
	if len(got) != len(want) {
		t.Fatalf("Cookies() = %#v, want %#v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Cookies()[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["bcookie"]; ok {
		t.Error("a cookie the jar dropped was resurrected from the static set")
	}
}

// TestCookiesWithoutSnapshotterReturnsStaticCopy covers a transport that
// doesn't implement CookieSnapshotter (e.g. a plain fixture transport in
// other tests): Cookies() must fall back to a copy of the static set rather
// than panicking or returning nothing.
func TestCookiesWithoutSnapshotterReturnsStaticCopy(t *testing.T) {
	st := &sequenceTransport{}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st))
	got := c.Cookies()
	want := map[string]string{"li_at": "li_at_test", "JSESSIONID": `"jsession_test"`}
	if len(got) != len(want) || got["li_at"] != want["li_at"] || got["JSESSIONID"] != want["JSESSIONID"] {
		t.Errorf("Cookies() = %#v, want %#v", got, want)
	}
	// The returned map must not alias the client's internal cookies.
	got["li_at"] = "mutated"
	if c.cookies["li_at"] != "li_at_test" {
		t.Errorf("Cookies() returned a map aliasing internal state; c.cookies[li_at] = %q", c.cookies["li_at"])
	}
}

// TestCookiesEmptySnapshotValueDoesNotEraseStored guards the specific
// footgun an overlay-by-default merge would otherwise have: a transport
// whose jar never saw a particular cookie rotate reports it back as "" (the
// zero value for a name it never observed), and that must never blank out a
// good cookie the static set already had.
func TestCookiesSkipsEmptySnapshotValues(t *testing.T) {
	// A jar entry with no value carries nothing worth persisting, so it is
	// dropped rather than written. auth.UpdateCookies then refuses the whole
	// set for lacking li_at, which is the safe outcome: better to keep the
	// stored credential and let the user re-authenticate than to write a
	// session-less record.
	st := &snapshotTransport{snapshot: map[string]string{"li_at": "", "lidc": "dc-2"}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st))
	got := c.Cookies()
	if _, ok := got["li_at"]; ok {
		t.Errorf("Cookies()[li_at] = %q, want the empty entry dropped", got["li_at"])
	}
	if got["lidc"] != "dc-2" {
		t.Errorf("Cookies()[lidc] = %q, want dc-2", got["lidc"])
	}
}

// F19: a POST that 500s must be attempted exactly once. Retrying a
// non-idempotent write whose response was merely lost in transit risks
// duplicating a sent message/invite/comment/post.
func TestPostNotRetriedOn500(t *testing.T) {
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 500, Body: []byte("boom")},
		{StatusCode: 200, Body: []byte("{}")}, // would only ever be reached if (incorrectly) retried
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	_, err := c.post(context.Background(), "/some/mutation", strings.NewReader("{}"), ratelimit.Write)
	if err == nil {
		t.Fatal("expected an error from the 500 response")
	}
	if st.calls != 1 {
		t.Errorf("POST attempted %d time(s), want exactly 1", st.calls)
	}
}

// F19 (other half): GET remains retried on 5xx, since it's idempotent.
func TestGetRetriedOn500ThenSucceeds(t *testing.T) {
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 500, Body: []byte("boom")},
		{StatusCode: 200, Body: []byte(`{"data":{}}`)},
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	body, err := c.get(context.Background(), "/some/read", nil)
	if err != nil {
		t.Fatalf("GET should have been retried and succeeded: %v", err)
	}
	if st.calls != 2 {
		t.Errorf("GET attempted %d time(s), want exactly 2 (one retry on 5xx)", st.calls)
	}
	if string(body) != `{"data":{}}` {
		t.Errorf("body = %q", body)
	}
}

// F22: a 403 with "checkpoint" in the body (not just "challenge") must also
// map to ErrChallenge, per DESIGN.md §4 exit code 7.
func TestChallenge403MatchesCheckpoint(t *testing.T) {
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 403, Body: []byte(`{"message":"Please complete this CheckPoint to continue"}`)},
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	_, err := c.get(context.Background(), "/me", nil)
	if !errors.Is(err, ErrChallenge) {
		t.Fatalf("want ErrChallenge, got %v", err)
	}
}

// F23: a session-expired redirect to a checkpoint page comes back as a
// plain 200 (both transports follow redirects); classifyRedirect must catch
// it via FinalURL rather than letting it fall through to a decode error.
func TestClassifyRedirectToCheckpoint(t *testing.T) {
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 200, Body: []byte("<html>checkpoint</html>"), FinalURL: "https://www.linkedin.com/checkpoint/challenge/?x=1"},
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	_, err := c.get(context.Background(), "/me", nil)
	if !errors.Is(err, ErrChallenge) {
		t.Fatalf("want ErrChallenge, got %v", err)
	}
}

// F23: a redirect to the login page must map to ErrUnauthorized (exit 3),
// not a generic decode error.
func TestClassifyRedirectToLogin(t *testing.T) {
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 200, Body: []byte("<html>login</html>"), FinalURL: "https://www.linkedin.com/uas/login?session_redirect=%2Ffeed"},
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	_, err := c.get(context.Background(), "/me", nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

// F23: a Set-Cookie that deletes li_at (DESIGN.md §3.3's documented
// "li_at=delete me; Expires=1970" wipe) must map to ErrUnauthorized even
// when the status code and final URL look otherwise normal.
func TestClassifySetCookieDeletesLiAt(t *testing.T) {
	hdr := http.Header{}
	hdr.Add("Set-Cookie", "li_at=delete me; Expires=Thu, 01 Jan 1970 00:00:00 GMT")
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 200, Body: []byte(`{"data":{}}`), FinalURL: "https://www.linkedin.com/voyager/api/me", Headers: hdr},
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	_, err := c.get(context.Background(), "/me", nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized (session-wipe cookie), got %v", err)
	}
}

// F23: a normal response (status, headers, and final URL all unremarkable)
// must not be reclassified — the checks are conservative by design.
func TestClassifyNormalResponseUnaffected(t *testing.T) {
	st := &sequenceTransport{responses: []*Response{
		{StatusCode: 200, Body: []byte(`{"data":{}}`), FinalURL: "https://www.linkedin.com/voyager/api/me"},
	}}
	c := New("li_at_test", `"jsession_test"`, WithTransport(st), WithLimiter(noopLimiter()))
	body, err := c.get(context.Background(), "/me", nil)
	if err != nil {
		t.Fatalf("normal response misclassified: %v", err)
	}
	if string(body) != `{"data":{}}` {
		t.Errorf("body = %q", body)
	}
}

// TestClassifyRedirectAllowsCustomBaseURL guards a regression: the redirect
// classifier once treated any final URL outside /voyager/api/ as an auth
// redirect, which turned every successful response served through
// WithBaseURL (httptest server, proxy, alternate endpoint) into
// ErrUnauthorized.
func TestClassifyRedirectAllowsCustomBaseURL(t *testing.T) {
	resp := &Response{
		StatusCode: 200,
		FinalURL:   "http://127.0.0.1:52341/me",
		Headers:    http.Header{},
	}
	if err := classifyRedirect(resp); err != nil {
		t.Fatalf("a success served from a custom base URL must not be reclassified, got %v", err)
	}
}

// TestClassifyRedirectStillCatchesAuthDestinations keeps the signals that do
// mean "not authenticated" working after narrowing the rule above.
func TestClassifyRedirectStillCatchesAuthDestinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want error
	}{
		{"checkpoint", "https://www.linkedin.com/checkpoint/challenge/", ErrChallenge},
		{"login", "https://www.linkedin.com/uas/login?goback=", ErrUnauthorized},
		{"authwall", "https://www.linkedin.com/authwall?trk=x", ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRedirect(&Response{StatusCode: 200, FinalURL: tc.url, Headers: http.Header{}})
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyRedirect(%s) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
	// Session-wipe cookie must still be caught regardless of URL.
	h := http.Header{}
	h.Add("Set-Cookie", `li_at=delete me; Expires=Thu, 01 Jan 1970 00:00:00 GMT`)
	if err := classifyRedirect(&Response{StatusCode: 200, FinalURL: "https://www.linkedin.com/voyager/api/me", Headers: h}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("li_at deletion should map to ErrUnauthorized, got %v", err)
	}
}
