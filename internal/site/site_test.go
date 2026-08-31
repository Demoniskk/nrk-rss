package site

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Demoniskk/nrk-rss/internal/store"
)

func seed(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "episodes.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	for _, p := range []store.Podcast{
		{ID: "p1", Title: "Podkast Én", Description: "Om & om igjen", CategoryName: "Humor", ImageURL: "https://img/1", LastScraped: time.Now().UTC()},
		{ID: "p2", Title: "Podkast To", CategoryName: "Nyheter", LastScraped: time.Now().UTC()},
		// Stored but with no episodes: must not get a feed file.
		{ID: "tom", Title: "Tom podkast", LastScraped: time.Now().UTC()},
	} {
		if err := st.UpsertPodcast(ctx, p); err != nil {
			t.Fatalf("UpsertPodcast: %v", err)
		}
	}

	eps := []store.Episode{
		{PodcastID: "p1", EpisodeID: "a", Title: "A", PubDate: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			EnclosureURL: "https://audio/a.mp3", EnclosureType: "audio/mpeg", EnclosureLength: 1, FetchedAt: time.Now()},
		{PodcastID: "p1", EpisodeID: "b", Title: "B", PubDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			EnclosureURL: "https://audio/b.mp3", EnclosureType: "audio/mpeg", EnclosureLength: 2, FetchedAt: time.Now()},
		{PodcastID: "p2", EpisodeID: "c", Title: "C", PubDate: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
			EnclosureURL: "https://audio/c.mp3", EnclosureType: "audio/mpeg", EnclosureLength: 3, FetchedAt: time.Now()},
	}
	if err := st.UpsertEpisodes(ctx, eps); err != nil {
		t.Fatalf("UpsertEpisodes: %v", err)
	}
	return st, dir
}

func TestExport(t *testing.T) {
	ctx := context.Background()
	st, dir := seed(t)
	out := filepath.Join(dir, "docs")

	m, err := Export(ctx, st, Options{
		OutDir:    out,
		BaseURL:   "https://example.github.io/nrk-rss/",
		Generator: "test",
		Failures:  map[string]string{"kaputt": "boom"},
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if m.PodcastCount != 2 || m.EpisodeCount != 3 {
		t.Errorf("manifest counts = %d podcasts / %d episodes, want 2/3 (the episode-less podcast is skipped)",
			m.PodcastCount, m.EpisodeCount)
	}
	if len(m.Failures) != 1 || m.Failures[0].ID != "kaputt" {
		t.Errorf("manifest failures = %+v, want the one reported failure", m.Failures)
	}
	byID := map[string]ManifestPodcast{}
	for _, p := range m.Podcasts {
		byID[p.ID] = p
	}
	if got := byID["p1"].FeedURL; got != "https://example.github.io/nrk-rss/feeds/p1.xml" {
		t.Errorf("FeedURL = %q; the trailing slash on BaseURL should not double up", got)
	}
	// Episodes are newest-first, so the manifest's latest_episode is the head.
	if got := byID["p1"].LatestEpisode; got != "2026-01-02T00:00:00Z" {
		t.Errorf("LatestEpisode = %q, want the newest episode's date", got)
	}
	if got := byID["p1"].EpisodeCount; got != 2 {
		t.Errorf("EpisodeCount = %d, want 2", got)
	}

	for _, name := range []string{"index.html", "feeds.json", ".nojekyll", "feeds/p1.xml", "feeds/p2.xml"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "feeds", "tom.xml")); err == nil {
		t.Error("wrote a feed for a podcast with no episodes")
	}

	// Every generated feed must be parseable XML.
	for _, name := range []string{"p1.xml", "p2.xml"} {
		b, err := os.ReadFile(filepath.Join(out, "feeds", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		var v any
		if err := xml.Unmarshal(b, &v); err != nil {
			t.Errorf("%s is not well-formed XML: %v", name, err)
		}
	}

	b, err := os.ReadFile(filepath.Join(out, "feeds.json"))
	if err != nil {
		t.Fatalf("reading feeds.json: %v", err)
	}
	var parsed Manifest
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("feeds.json is not valid JSON: %v", err)
	}
	if parsed.GeneratedAt == "" || len(parsed.Podcasts) != 2 {
		t.Errorf("feeds.json round-trip = %+v", parsed)
	}

	html, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	page := string(html)
	for _, want := range []string{
		"Podkast Én", `href="feeds/p1.xml"`, `id="q"`,
		// The description is shown on the card, not just carried in feeds.json.
		`class="desc"`, "Om &amp; om igjen",
		// Mobile: a viewport that scales to the device, and the breakpoint
		// that moves the action buttons onto their own row.
		`name="viewport"`, "@media (max-width: 34rem)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// html/template must escape the ampersand rather than emit it raw.
	if strings.Contains(page, "Om & om") {
		t.Error("index.html contains an unescaped ampersand")
	}
	// p2 has no description, so it must not get an empty description element.
	if strings.Count(page, `class="desc"`) != 1 {
		t.Errorf("expected exactly one rendered description, got %d",
			strings.Count(page, `class="desc"`))
	}
}

func TestSummarise(t *testing.T) {
	long := strings.Repeat("ærlig ", 200) // multi-byte, well over the limit

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"collapses whitespace", "Rett etter\n  terroren\tkom", "Rett etter terroren kom"},
		{"leaves short text alone", "Kort.", "Kort."},
		{"trims trailing empty lines", "  Hei  ", "Hei"},
	}
	for _, tt := range tests {
		if got := summarise(tt.in); got != tt.want {
			t.Errorf("%s: summarise(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}

	got := summarise(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated summary should end in an ellipsis, got %q", got)
	}
	// The limit counts runes, so a Norwegian description must not be cut
	// roughly in half just because its characters are two bytes wide.
	if n := len([]rune(got)); n > summaryLimit+1 || n < summaryLimit/2 {
		t.Errorf("truncated summary is %d runes, want about %d", n, summaryLimit)
	}
	if !utf8.ValidString(got) {
		t.Error("truncation split a multi-byte character")
	}
}

func TestExportPrunesStaleFeeds(t *testing.T) {
	ctx := context.Background()
	st, dir := seed(t)
	out := filepath.Join(dir, "docs")

	feedsDir := filepath.Join(out, "feeds")
	if err := os.MkdirAll(feedsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(feedsDir, "delisted.xml")
	if err := os.WriteFile(stale, []byte("<rss/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-feed file in the same directory must be left alone.
	keep := filepath.Join(feedsDir, "notes.txt")
	if err := os.WriteFile(keep, []byte("hei"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Export(ctx, st, Options{OutDir: out}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	if _, err := os.Stat(stale); err == nil {
		t.Error("stale feed for a delisted podcast was not removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("pruning removed an unrelated file: %v", err)
	}
}
