package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state", "episodes.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEpisodeRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	pub := time.Date(2026, 1, 15, 5, 0, 0, 0, time.FixedZone("CET", 3600))
	in := Episode{
		PodcastID:       "p1",
		EpisodeID:       "l_a",
		Title:           "Tittel",
		Description:     "Beskrivelse",
		PubDate:         pub,
		DurationSeconds: 2052,
		EnclosureURL:    "https://podkast.nrk.no/a.mp3",
		EnclosureType:   "audio/mpeg",
		EnclosureLength: 12345678,
		ImageURL:        "https://gfx.nrk.no/a",
		SeasonID:        "1",
		SeasonName:      "Sesong 1",
		IndexPoints:     []IndexPoint{{Title: "Kapittel", StartSeconds: 90}},
		Contributors:    []Contributor{{Role: "Programleder", Name: []string{"Ola", "Kari"}}},
		FetchedAt:       time.Now().UTC(),
	}

	if err := st.UpsertEpisodes(ctx, []Episode{in}); err != nil {
		t.Fatalf("UpsertEpisodes: %v", err)
	}

	got, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1", len(got))
	}
	e := got[0]

	if !e.PubDate.Equal(pub) {
		t.Errorf("PubDate = %v, want %v", e.PubDate, pub)
	}
	if e.Title != in.Title || e.EnclosureLength != in.EnclosureLength || e.SeasonName != in.SeasonName {
		t.Errorf("scalar round-trip mismatch: %+v", e)
	}
	if len(e.IndexPoints) != 1 || e.IndexPoints[0].StartSeconds != 90 {
		t.Errorf("IndexPoints = %+v, want one marker at 90s", e.IndexPoints)
	}
	if len(e.Contributors) != 1 || len(e.Contributors[0].Name) != 2 {
		t.Errorf("Contributors = %+v, want one credit with two names", e.Contributors)
	}
}

func TestUpsertIsIdempotentAndUpdates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	base := Episode{
		PodcastID: "p1", EpisodeID: "l_a", Title: "Før",
		PubDate: time.Now(), EnclosureURL: "u", EnclosureType: "audio/mpeg",
		FetchedAt: time.Now(),
	}
	if err := st.UpsertEpisodes(ctx, []Episode{base}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	base.Title = "Etter"
	base.EnclosureLength = 99
	if err := st.UpsertEpisodes(ctx, []Episode{base}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d episodes, want 1 after re-upsert", len(got))
	}
	if got[0].Title != "Etter" || got[0].EnclosureLength != 99 {
		t.Errorf("upsert did not update the row: %+v", got[0])
	}
}

func TestEpisodesAreOrderedNewestFirst(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	mk := func(id string, day int) Episode {
		return Episode{
			PodcastID: "p1", EpisodeID: id, Title: id,
			PubDate:       time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
			EnclosureURL:  "u",
			EnclosureType: "audio/mpeg",
			FetchedAt:     time.Now(),
		}
	}
	if err := st.UpsertEpisodes(ctx, []Episode{mk("a", 1), mk("c", 3), mk("b", 2)}); err != nil {
		t.Fatalf("UpsertEpisodes: %v", err)
	}

	got, err := st.EpisodesForPodcast(ctx, "p1")
	if err != nil {
		t.Fatalf("EpisodesForPodcast: %v", err)
	}
	want := []string{"c", "b", "a"}
	for i, w := range want {
		if got[i].EpisodeID != w {
			t.Fatalf("order = %s..., want %v", got[i].EpisodeID, want)
		}
	}
}

func TestKnownEpisodeIDsIsPerPodcast(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	eps := []Episode{
		{PodcastID: "p1", EpisodeID: "a", Title: "a", PubDate: time.Now(), EnclosureURL: "u", EnclosureType: "t", FetchedAt: time.Now()},
		{PodcastID: "p1", EpisodeID: "b", Title: "b", PubDate: time.Now(), EnclosureURL: "u", EnclosureType: "t", FetchedAt: time.Now()},
		{PodcastID: "p2", EpisodeID: "c", Title: "c", PubDate: time.Now(), EnclosureURL: "u", EnclosureType: "t", FetchedAt: time.Now()},
	}
	if err := st.UpsertEpisodes(ctx, eps); err != nil {
		t.Fatalf("UpsertEpisodes: %v", err)
	}

	known, err := st.KnownEpisodeIDs(ctx, "p1")
	if err != nil {
		t.Fatalf("KnownEpisodeIDs: %v", err)
	}
	if len(known) != 2 || !known["a"] || !known["b"] || known["c"] {
		t.Errorf("known = %v, want exactly {a, b}", known)
	}

	counts, err := st.EpisodeCounts(ctx)
	if err != nil {
		t.Fatalf("EpisodeCounts: %v", err)
	}
	if counts["p1"] != 2 || counts["p2"] != 1 {
		t.Errorf("counts = %v, want p1=2 p2=1", counts)
	}
}

func TestPodcastUpsert(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	p := Podcast{ID: "p1", Title: "Én", CategoryName: "Humor", LastScraped: time.Now().UTC()}
	if err := st.UpsertPodcast(ctx, p); err != nil {
		t.Fatalf("UpsertPodcast: %v", err)
	}
	p.Title = "To"
	if err := st.UpsertPodcast(ctx, p); err != nil {
		t.Fatalf("UpsertPodcast (update): %v", err)
	}

	got, err := st.AllPodcasts(ctx)
	if err != nil {
		t.Fatalf("AllPodcasts: %v", err)
	}
	if len(got) != 1 || got[0].Title != "To" {
		t.Errorf("AllPodcasts = %+v, want a single podcast titled To", got)
	}
}

// Re-upserting identical metadata must not move last_scraped. Otherwise every
// run rewrites the committed database and the daily job can never skip its
// commit, which is the whole reason the update/scrape split exists.
func TestUnchangedPodcastUpsertIsANoOp(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	p := Podcast{
		ID: "p1", Title: "Tittel", Description: "Beskrivelse",
		CategoryID: "humor", CategoryName: "Humor", ImageURL: "https://img/1",
		LastScraped: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := st.UpsertPodcast(ctx, p); err != nil {
		t.Fatalf("UpsertPodcast: %v", err)
	}

	// Same content, later timestamp: the timestamp alone must not win.
	later := p
	later.LastScraped = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := st.UpsertPodcast(ctx, later); err != nil {
		t.Fatalf("UpsertPodcast (no-op): %v", err)
	}

	got, err := st.AllPodcasts(ctx)
	if err != nil {
		t.Fatalf("AllPodcasts: %v", err)
	}
	if !got[0].LastScraped.Equal(p.LastScraped) {
		t.Errorf("LastScraped = %v, want it unchanged at %v", got[0].LastScraped, p.LastScraped)
	}

	// A real metadata change must still be recorded, timestamp included.
	changed := later
	changed.Title = "Ny tittel"
	if err := st.UpsertPodcast(ctx, changed); err != nil {
		t.Fatalf("UpsertPodcast (real change): %v", err)
	}
	got, err = st.AllPodcasts(ctx)
	if err != nil {
		t.Fatalf("AllPodcasts: %v", err)
	}
	if got[0].Title != "Ny tittel" || !got[0].LastScraped.Equal(changed.LastScraped) {
		t.Errorf("a real change was not recorded: %+v", got[0])
	}
}
