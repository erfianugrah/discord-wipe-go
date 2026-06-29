package discord

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// testClient returns a Client whose baseURL points at the given test server.
func testClient(srv *httptest.Server) *Client {
	c := NewClient("test-token")
	c.baseURL = srv.URL
	return c
}

// SAFETY MANDATE 1 (only-my-messages): every search MUST be author-filtered
// with the caller's own ID so Discord server-side-filters to our messages.
// A regression here would let an admin account enumerate everyone's messages.
func TestSearchAlwaysSendsAuthorID(t *testing.T) {
	var gotAuthor string
	var gotMaxID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthor = r.URL.Query().Get("author_id")
		gotMaxID = r.URL.Query().Get("max_id")
		fmt.Fprint(w, `{"total_results":0,"messages":[]}`)
	}))
	defer srv.Close()

	c := testClient(srv)
	_, _, _, err := c.SearchMessages("guild", "G1", SearchParams{AuthorID: "ME123", MaxID: 999})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuthor != "ME123" {
		t.Fatalf("author_id=%q, want ME123 (search must be author-filtered)", gotAuthor)
	}
	if gotMaxID != "999" {
		t.Fatalf("max_id=%q, want 999 (retention bound must be sent)", gotMaxID)
	}
}

// SAFETY MANDATE 2 (only-my-messages defence in depth): a 403 on DELETE is
// terminal "forbidden" and must NEVER be retried — it's the last line of
// defence if a code path ever produced a message ID we don't own.
func TestDeleteForbiddenOn403NeverRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(403)
	}))
	defer srv.Close()

	res, err := testClient(srv).DeleteMessage("C1", "M1")
	if err != nil {
		t.Fatalf("403 must not be an error, got %v", err)
	}
	if res.Status != "forbidden" {
		t.Fatalf("status=%q, want forbidden", res.Status)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("made %d requests on 403, want exactly 1 (must not retry)", n)
	}
}

// A mid-pass 401 must surface as a typed AuthError, NOT a panic. The previous
// code panicked with no recover() anywhere, crashing the daemon on token
// rotation instead of parking.
func TestDelete401ReturnsAuthErrorNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"message":"401: Unauthorized"}`)
	}))
	defer srv.Close()

	res, err := testClient(srv).DeleteMessage("C1", "M1")
	if _, ok := err.(*AuthError); !ok {
		t.Fatalf("err=%v (%T), want *AuthError", err, err)
	}
	if res.Status != "auth" {
		t.Fatalf("status=%q, want auth", res.Status)
	}
}

func TestSearch401ReturnsAuthErrorNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"message":"401: Unauthorized"}`)
	}))
	defer srv.Close()

	_, _, _, err := testClient(srv).SearchMessages("guild", "G1", SearchParams{AuthorID: "ME"})
	if _, ok := err.(*AuthError); !ok {
		t.Fatalf("err=%v (%T), want *AuthError", err, err)
	}
}

func TestDeleteStatusMapping(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{204, "ok"},
		{404, "gone"},
		{403, "forbidden"},
		{400, "forbidden"},
		{500, "retry"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
		}))
		res, err := testClient(srv).DeleteMessage("C1", "M1")
		srv.Close()
		if err != nil {
			t.Fatalf("code %d: unexpected error %v", tc.code, err)
		}
		if res.Status != tc.want {
			t.Fatalf("code %d: status=%q, want %q", tc.code, res.Status, tc.want)
		}
	}
}

func TestDelete429ReturnsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"retry_after":3.5}`)
	}))
	defer srv.Close()

	res, err := testClient(srv).DeleteMessage("C1", "M1")
	if err != nil {
		t.Fatalf("429 must not be an error, got %v", err)
	}
	if res.Status != "retry" || res.RetryAfter != 3.5 {
		t.Fatalf("got status=%q retry=%v, want retry/3.5", res.Status, res.RetryAfter)
	}
}

func TestBucketPacing(t *testing.T) {
	mk := func(remaining, resetAfter string) *http.Response {
		h := http.Header{}
		if remaining != "" {
			h.Set("X-RateLimit-Remaining", remaining)
		}
		if resetAfter != "" {
			h.Set("X-RateLimit-Reset-After", resetAfter)
		}
		return &http.Response{Header: h}
	}
	if got := bucketPacing(mk("0", "5")); got != 5 {
		t.Fatalf("remaining=0 reset=5: got %v, want 5 (full wait)", got)
	}
	if got := bucketPacing(mk("5", "5")); got != 1 {
		t.Fatalf("remaining=5 reset=5: got %v, want 1 (reset/remaining)", got)
	}
	if got := bucketPacing(mk("", "")); got != 0 {
		t.Fatalf("no headers: got %v, want 0", got)
	}
}
