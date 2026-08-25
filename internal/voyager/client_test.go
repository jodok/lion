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
