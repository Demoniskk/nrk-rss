package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Demoniskk/nrk-rss/internal/nrkapi"
	"github.com/Demoniskk/nrk-rss/internal/store"
)

// fakeNRK is a stand-in for psapi.nrk.no that serves a configurable episode
// list and counts the requests each endpoint receives, so tests can assert on
// how much work a run actually did.
type fakeNRK struct {
	mu sync.Mutex
	// episodes are newest-first, as NRK returns them.
	episodes []fakeEpisode
	pageSize int
	// unplayable episode IDs report playability != "playable".
	unplayable map[string]bool

	counts map[string]int
	server *httptest.Server
}

type fakeEpisode struct {
	ID    string
	Title string
	Date  time.Time
}

func newFakeNRK(t *testing.T, episodes []fakeEpisode) *fakeNRK {
	t.Helper()
	f := &fakeNRK{
		episodes:   episodes,
		pageSize:   50,
		unplayable: map[string]bool{},
		counts:     map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeNRK) count(key string) {
	f.mu.Lock()
	f.counts[key]++
	f.mu.Unlock()
}

func (f *fakeNRK) get(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[key]
}

func (f *fakeNRK) setEpisodes(eps []fakeEpisode) {
	f.mu.Lock()
	f.episodes = eps
	f.mu.Unlock()
}

func (f *fakeNRK) snapshot() []fakeEpisode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeEpisode(nil), f.episodes...)
}

func (f *fakeNRK) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	w.Header().Set("Content-Type", "application/json")

	switch {
	case path == "/radio/search/categories/podcast":
		f.count("search")
		writeJSON(w, map[string]any{"series": []map[string]any{
			{"id": "p1", "type": "podcast", "title": "Podkast Én"},
			// Not a podcast in its own right; must be filtered out.
			{"id": "s1", "type": "customSeason", "title": "En sesong"},
		}})

	case path == "/radio/catalog/podcast/p1":
		f.count("meta")
		writeJSON(w, map[string]any{
			"type": "podcast", "seriesType": "standard",
			"series": map[string]any{
				"id":       "p1",
				"titles":   map[string]any{"title": "Podkast Én", "subtitle": "Beskrivelse"},
				"category": map[string]any{"id": "humor", "name": "Humor"},
				"squareImage": []map[string]any{
					{"url": "https://img/small", "width": 300},
					{"url": "https://img/big", "width": 1920},
				},
			},
		})

	case path == "/radio/catalog/podcast/p1/episodes":
		f.count("episodes")
		f.serveEpisodePage(w, r)

	case strings.HasPrefix(path, "/radio/catalog/podcast/p1/episodes/"):
		f.count("episode-detail")
		id := strings.TrimPrefix(path, "/radio/catalog/podcast/p1/episodes/")
		writeJSON(w, f.episodeJSON(id, "Detaljert", time.Now(), true))

	case strings.HasPrefix(path, "/playback/manifest/podcast/"):
		f.count("manifest")
		id := strings.TrimPrefix(path, "/playback/manifest/podcast/")
		f.mu.Lock()
		bad := f.unplayable[id]
		f.mu.Unlock()
		if bad {
			writeJSON(w, map[string]any{"playability": "nonPlayable", "playable": nil})
			return
		}
		writeJSON(w, map[string]any{
			"playability": "playable",
			"playable": map[string]any{"assets": []map[string]any{
				{"url": f.server.URL + "/audio/" + id + ".mp3", "format": "MP3", "mimeType": "audio/mp3"},
			}},
		})

	case strings.HasPrefix(path, "/audio/"):
		f.count("head")
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "4242")
		w.WriteHeader(http.StatusOK)

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNRK) serveEpisodePage(w http.ResponseWriter, r *http.Request) {
	eps := f.snapshot()

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if size < 1 {
		size = f.pageSize
	}

	start := (page - 1) * size
	if start > len(eps) {
		start = len(eps)
	}
	end := min(start+size, len(eps))

	items := make([]map[string]any, 0, end-start)
	for _, e := range eps[start:end] {
		items = append(items, f.episodeJSON(e.ID, e.Title, e.Date, false))
	}

	body := map[string]any{
		"_embedded": map[string]any{"episodes": items},
		"_links":    map[string]any{},
	}
	if end < len(eps) {
		body["_links"] = map[string]any{
			"next": map[string]any{"href": fmt.Sprintf("/episodes?page=%d", page+1)},
		}
	}
	writeJSON(w, body)
}

func (f *fakeNRK) episodeJSON(id, title string, date time.Time, rich bool) map[string]any {
	m := map[string]any{
		"id":        "internal-" + id,
		"episodeId": id,
		"titles":    map[string]any{"title": title, "subtitle": "Undertittel " + id},
		"duration":  "PT34M12S",
		"date":      date.Format(time.RFC3339),
		"squareImage": []map[string]any{
			{"url": "https://img/" + id, "width": 1080},
		},
		"_links": map[string]any{
			"season": map[string]any{"name": "2026", "title": "Sesong 2026"},
		},
	}
	if rich {
		m["indexPoints"] = []map[string]any{
			{"title": "Kapittel", "startPoint": "PT1M30S", "partId": 0},
		}
		m["contributors"] = []map[string]any{
			{"role": "Programleder", "name": []string{"Ola Nordmann"}},
		}
	}
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func newHarness(t *testing.T, eps []fakeEpisode) (*fakeNRK, *Scraper, *store.Store) {
	t.Helper()

	fake := newFakeNRK(t, eps)

	st, err := store.Open(filepath.Join(t.TempDir(), "episodes.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := nrkapi.New(log)
	client.BaseURL = fake.server.URL
	client.MaxAttempts = 1
	// No pacing against a local test server; the live default would make these
	// tests take minutes.
	client.SetRate(0, 0)

	return fake, New(client, st, log), st
}

func makeEpisodes(n int) []fakeEpisode {
	eps := make([]fakeEpisode, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range eps {
		// Newest first, matching NRK's ordering.
		eps[i] = fakeEpisode{
			ID:    fmt.Sprintf("l_%03d", n-i),
			Title: fmt.Sprintf("Episode %d", n-i),
			Date:  base.AddDate(0, 0, n-i),
		}
	}
	return eps
}

func TestFullScrapePagesThroughEverything(t *testing.T) {
	ctx := context.Background()
	fake, sc, st := newHarness(t, makeEpisodes(120))

	res, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.PodcastsSeen != 1 {
		t.Errorf("PodcastsSeen = %d, want 1 (customSeason entries must be filtered out)", res.PodcastsSeen)
	}
	if res.EpisodesAdded != 120 {
		t.Errorf("EpisodesAdded = %d, want 120", res.EpisodesAdded)
	}
	// 120 episodes at 50 per page is three pages.
	if got := fake.get("episodes"); got != 3 {
		t.Errorf("episode-list requests = %d, want 3", got)
	}
	if got := fake.get("manifest"); got != 120 {
		t.Errorf("manifest requests = %d, want 120", got)
	}
	if got := fake.get("head"); got != 120 {
		t.Errorf("HEAD requests = %d, want 120", got)
	}
	// The default path must not pay for the episode-detail call.
	if got := fake.get("episode-detail"); got != 0 {
		t.Errorf("episode-detail requests = %d, want 0 without --rich", got)
	}

	stored, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	if len(stored) != 120 {
		t.Fatalf("stored %d episodes, want 120", len(stored))
	}
	if stored[0].EnclosureType != "audio/mpeg" {
		t.Errorf("EnclosureType = %q, want audio/mpeg (normalised from the manifest's audio/mp3)", stored[0].EnclosureType)
	}
	if stored[0].EnclosureLength != 4242 {
		t.Errorf("EnclosureLength = %d, want 4242 from the HEAD response", stored[0].EnclosureLength)
	}
	if stored[0].ImageURL == "" || stored[0].DurationSeconds != 2052 {
		t.Errorf("episode metadata not carried over: %+v", stored[0])
	}
}

func TestUpdateFetchesOnlyNewEpisodes(t *testing.T) {
	ctx := context.Background()
	fake, sc, st := newHarness(t, makeEpisodes(120))

	if _, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 50}); err != nil {
		t.Fatalf("initial scrape: %v", err)
	}
	before := struct{ manifest, head, episodes int }{
		fake.get("manifest"), fake.get("head"), fake.get("episodes"),
	}

	// NRK publishes two new episodes at the front of the list.
	old := fake.snapshot()
	newer := append([]fakeEpisode{
		{ID: "l_new2", Title: "Ny 2", Date: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)},
		{ID: "l_new1", Title: "Ny 1", Date: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
	}, old...)
	fake.setEpisodes(newer)

	res, err := sc.Run(ctx, Options{Mode: ModeIncremental, PageSize: 50, MaxPagesPerPodcast: 3})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if res.EpisodesAdded != 2 {
		t.Errorf("EpisodesAdded = %d, want 2", res.EpisodesAdded)
	}
	// Exactly the two new episodes get the manifest + HEAD pair; the other
	// 120 must not be touched at all.
	if got := fake.get("manifest") - before.manifest; got != 2 {
		t.Errorf("manifest requests during update = %d, want 2", got)
	}
	if got := fake.get("head") - before.head; got != 2 {
		t.Errorf("HEAD requests during update = %d, want 2", got)
	}
	// One page is enough: the first page already contains a known episode.
	if got := fake.get("episodes") - before.episodes; got != 1 {
		t.Errorf("episode-list requests during update = %d, want 1", got)
	}

	stored, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	if len(stored) != 122 {
		t.Errorf("stored %d episodes, want 122", len(stored))
	}
	if stored[0].EpisodeID != "l_new2" {
		t.Errorf("newest stored episode = %q, want l_new2", stored[0].EpisodeID)
	}
}

// A podcast with nothing stored is new to us, so an incremental run must
// backfill it completely rather than stopping at the page cap and leaving a
// permanent hole no later update would fill.
func TestUpdateBackfillsPodcastsWithNoStoredEpisodes(t *testing.T) {
	ctx := context.Background()
	fake, sc, _ := newHarness(t, makeEpisodes(120))

	res, err := sc.Run(ctx, Options{Mode: ModeIncremental, PageSize: 10, MaxPagesPerPodcast: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.EpisodesAdded != 120 {
		t.Errorf("EpisodesAdded = %d, want all 120 despite the 2-page cap", res.EpisodesAdded)
	}
	if got := fake.get("episodes"); got != 12 {
		t.Errorf("episode-list requests = %d, want 12 (the cap must not apply)", got)
	}
}

// Once a podcast is known, the cap does apply, so a podcast that replaced its
// whole catalogue cannot make one run page through everything.
func TestUpdateRespectsPageCapForKnownPodcasts(t *testing.T) {
	ctx := context.Background()
	fake, sc, _ := newHarness(t, makeEpisodes(10))

	if _, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 10}); err != nil {
		t.Fatalf("initial scrape: %v", err)
	}
	beforePages := fake.get("episodes")

	// Every episode is replaced, so no known ID is ever hit.
	fake.setEpisodes(makeEpisodes(200)[:100])

	if _, err := sc.Run(ctx, Options{Mode: ModeIncremental, PageSize: 10, MaxPagesPerPodcast: 2}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := fake.get("episodes") - beforePages; got != 2 {
		t.Errorf("episode-list requests during update = %d, want the 2-page cap", got)
	}
}

func TestUnplayableEpisodesAreSkippedNotFatal(t *testing.T) {
	ctx := context.Background()
	fake, sc, st := newHarness(t, makeEpisodes(5))

	fake.mu.Lock()
	fake.unplayable["l_003"] = true
	fake.mu.Unlock()

	res, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PodcastsFailed != 0 {
		t.Errorf("PodcastsFailed = %d, want 0: an unplayable episode must not fail its podcast", res.PodcastsFailed)
	}
	if res.EpisodesAdded != 4 || res.EpisodesSkipped != 1 {
		t.Errorf("added/skipped = %d/%d, want 4/1", res.EpisodesAdded, res.EpisodesSkipped)
	}

	stored, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	for _, e := range stored {
		if e.EpisodeID == "l_003" {
			t.Error("unplayable episode was stored")
		}
	}
}

func TestForceRefetchesEverything(t *testing.T) {
	ctx := context.Background()
	fake, sc, _ := newHarness(t, makeEpisodes(5))

	if _, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 50}); err != nil {
		t.Fatalf("initial scrape: %v", err)
	}
	before := fake.get("manifest")

	res, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 50, Force: true})
	if err != nil {
		t.Fatalf("forced scrape: %v", err)
	}
	if res.EpisodesAdded != 5 {
		t.Errorf("EpisodesAdded = %d, want 5 with --force", res.EpisodesAdded)
	}
	if got := fake.get("manifest") - before; got != 5 {
		t.Errorf("manifest requests during forced run = %d, want 5", got)
	}
}

func TestRichModeAddsChaptersAndCredits(t *testing.T) {
	ctx := context.Background()
	fake, sc, st := newHarness(t, makeEpisodes(3))

	if _, err := sc.Run(ctx, Options{Mode: ModeFull, PageSize: 50, Rich: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fake.get("episode-detail"); got != 3 {
		t.Errorf("episode-detail requests = %d, want 3 with Rich", got)
	}

	stored, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	if len(stored) == 0 || len(stored[0].IndexPoints) != 1 || stored[0].IndexPoints[0].StartSeconds != 90 {
		t.Errorf("chapters not stored: %+v", stored)
	}
	if len(stored[0].Contributors) != 1 {
		t.Errorf("contributors not stored: %+v", stored[0].Contributors)
	}
}

func TestOnlyRestrictsTheRun(t *testing.T) {
	ctx := context.Background()
	fake, sc, _ := newHarness(t, makeEpisodes(3))

	res, err := sc.Run(ctx, Options{Mode: ModeFull, Only: []string{"does-not-exist"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PodcastsSeen != 0 {
		t.Errorf("PodcastsSeen = %d, want 0", res.PodcastsSeen)
	}
	if got := fake.get("meta"); got != 0 {
		t.Errorf("metadata requests = %d, want 0", got)
	}
}
