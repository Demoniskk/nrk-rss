package nrkapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.BaseURL = baseURL
	c.SetRate(0, 0) // no pacing against a local server
	c.BaseBackoff = time.Millisecond
	c.MaxRetryWait = 50 * time.Millisecond
	return c
}

func TestParseRetryAfter(t *testing.T) {
	max := time.Minute
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{" 5 ", 5 * time.Second},
		// NRK asks for 600s; the cap keeps a run from stalling on it.
		{"600", time.Minute},
		{"-1", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in, max); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// A zero max means no cap.
	if got := parseRetryAfter("600", 0); got != 600*time.Second {
		t.Errorf("parseRetryAfter with no cap = %v, want 600s", got)
	}
}

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"series":{"id":"p1","titles":{"title":"Ok"}}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	got, err := c.GetPodcast(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetPodcast: %v", err)
	}
	if got.Series.Titles.Title != "Ok" {
		t.Errorf("title = %q, want Ok", got.Series.Titles.Title)
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("server saw %d requests, want 3 (two 429s then success)", n)
	}
}

// A 429 received by one worker must hold back every other worker, not just the
// one that got it. Backing off a single goroutine is useless against NRK, whose
// limiter stays rate-limited while the remaining workers keep sending.
//
// Requests already on the wire cannot be recalled, so the guarantee is about
// requests that start after the 429: those must wait out the pause.
func TestRateLimitPausesOtherWorkers(t *testing.T) {
	const pause = 300 * time.Millisecond

	var (
		limitNext atomic.Bool
		limited   = make(chan struct{})
		once      sync.Once
	)
	limitNext.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limitNext.CompareAndSwap(true, false) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			once.Do(func() { close(limited) })
			return
		}
		w.Write([]byte(`{"series":{"id":"p1","titles":{"title":"Ok"}}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	c.MaxRetryWait = pause

	// Worker A trips the limiter. It is waited on before the test returns, so
	// its retry cannot outlive the server.
	var workerA sync.WaitGroup
	workerA.Add(1)
	go func() {
		defer workerA.Done()
		if _, err := c.GetPodcast(context.Background(), "p1"); err != nil {
			t.Errorf("worker A: %v", err)
		}
	}()
	defer workerA.Wait()

	<-limited
	// Give the client a moment to record the penalty the 429 carried.
	time.Sleep(20 * time.Millisecond)

	// Worker B starts now, and must be held back by A's 429.
	start := time.Now()
	if _, err := c.GetPodcast(context.Background(), "p1"); err != nil {
		t.Fatalf("worker B: %v", err)
	}
	waited := time.Since(start)

	// Allow generous slack for scheduling; the point is that B waited at all
	// rather than sailing straight through.
	if waited < pause/2 {
		t.Errorf("a request started after another worker's 429 waited only %v, want at least %v",
			waited, pause/2)
	}
}

func TestNotFoundIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.GetPodcast(context.Background(), "gone")
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	if !isNotFound(err) {
		t.Errorf("error %v does not match ErrNotFound", err)
	}
}

func isNotFound(err error) bool {
	for err != nil {
		if err == ErrNotFound {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestAudioAssetRejectsUnplayable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"playability":"nonPlayable","playable":null}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	if _, err := c.AudioAsset(context.Background(), "l_x"); err == nil {
		t.Fatal("expected an error for an unplayable episode")
	}
}

func TestHeadAssetReadsSizeAndType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "289772")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	info, err := c.HeadAsset(context.Background(), srv.URL+"/a.mp3")
	if err != nil {
		t.Fatalf("HeadAsset: %v", err)
	}
	if info.Length != 289772 || info.Type != "audio/mpeg" {
		t.Errorf("AssetInfo = %+v, want 289772 / audio/mpeg", info)
	}
}

// A CDN node that omits Content-Length must not cost us the episode.
func TestHeadAssetToleratesMissingLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	info, err := c.HeadAsset(context.Background(), srv.URL+"/a.mp3")
	if err != nil {
		t.Fatalf("HeadAsset: %v", err)
	}
	if info.Length != 0 || info.Type != "audio/mpeg" {
		t.Errorf("AssetInfo = %+v, want length 0 and a usable type", info)
	}
}

// NRK serves at most 50 episodes per page and answers anything larger with a
// non-retryable 400, so an over-large --page-size must be clamped rather than
// failing every podcast in the run.
func TestEpisodePageClampsPageSize(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("pageSize")
		w.Write([]byte(`{"_embedded":{"episodes":[]},"_links":{}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	for _, in := range []int{0, -1, 51, 500, 10000} {
		if _, err := c.EpisodePage(context.Background(), "p1", 1, in); err != nil {
			t.Fatalf("EpisodePage(pageSize=%d): %v", in, err)
		}
		if got != "50" {
			t.Errorf("pageSize=%d was sent as %q, want 50", in, got)
		}
	}

	// A legal value must be passed through untouched.
	if _, err := c.EpisodePage(context.Background(), "p1", 1, 20); err != nil {
		t.Fatalf("EpisodePage(20): %v", err)
	}
	if got != "20" {
		t.Errorf("pageSize=20 was sent as %q, want 20", got)
	}
}

func TestListPodcastsFiltersNonPodcasts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"series":[
			{"id":"a","type":"podcast","title":"A"},
			{"id":"b","type":"customSeason","title":"B"},
			{"id":"c","type":"series","title":"C"},
			{"id":"a","type":"podcast","title":"A duplicate"},
			{"id":"","type":"podcast","title":"No id"}
		]}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	got, err := c.ListPodcasts(context.Background())
	if err != nil {
		t.Fatalf("ListPodcasts: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("ListPodcasts = %+v, want just the one real, de-duplicated podcast", got)
	}
}
