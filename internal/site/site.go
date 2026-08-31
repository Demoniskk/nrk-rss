// Package site exports the state store to the static files published on
// GitHub Pages: one RSS feed per podcast, a machine-readable manifest, and an
// index page. Nothing here touches the NRK API.
package site

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Demoniskk/nrk-rss/internal/feed"
	"github.com/Demoniskk/nrk-rss/internal/store"
)

// Options configures an export.
type Options struct {
	// OutDir is the directory published to Pages, conventionally "docs".
	OutDir string
	// BaseURL is the public root the site is served from, e.g.
	// "https://user.github.io/nrk-podcast-rss". Used for absolute feed URLs;
	// when empty, feed URLs in the manifest are site-relative and the
	// atom:self link is omitted.
	BaseURL string
	// Generator names this tool in generated feeds.
	Generator string
	// Failures maps podcast ID to the error message from the run that just
	// finished, for inclusion in the manifest.
	Failures map[string]string
	Logger   *slog.Logger
}

// Manifest is the schema of feeds.json.
type Manifest struct {
	// GeneratedAt is when this run produced the manifest. It is the one field
	// that changes on every run, so it is excluded from the comparison that
	// decides whether feeds.json needs rewriting at all.
	GeneratedAt string `json:"generated_at"`
	// LatestEpisode is the newest publication date across the whole
	// catalogue. Unlike GeneratedAt it only moves when there is new content,
	// which makes it the right thing to show on the index page.
	LatestEpisode string            `json:"latest_episode,omitempty"`
	Source        string            `json:"source"`
	PodcastCount  int               `json:"podcast_count"`
	EpisodeCount  int               `json:"episode_count"`
	Podcasts      []ManifestPodcast `json:"podcasts"`
	Failures      []ManifestFailure `json:"failures"`
}

// ManifestPodcast is one entry in feeds.json.
type ManifestPodcast struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description,omitempty"`
	Category      string `json:"category,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	EpisodeCount  int    `json:"episode_count"`
	FeedURL       string `json:"feed_url"`
	NRKURL        string `json:"nrk_url"`
	LatestEpisode string `json:"latest_episode,omitempty"`
}

// ManifestFailure records a podcast that could not be scraped this run.
type ManifestFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Export writes every feed, feeds.json and index.html from what is currently
// in the store.
func Export(ctx context.Context, st *store.Store, opts Options) (*Manifest, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.OutDir == "" {
		opts.OutDir = "docs"
	}

	feedsDir := filepath.Join(opts.OutDir, "feeds")
	if err := os.MkdirAll(feedsDir, 0o755); err != nil {
		return nil, fmt.Errorf("site: creating %s: %w", feedsDir, err)
	}

	podcasts, err := st.AllPodcasts(ctx)
	if err != nil {
		return nil, err
	}

	generatedAt := time.Now().UTC()
	manifest := &Manifest{
		GeneratedAt: generatedAt.Format(time.RFC3339),
		Source:      "https://radio.nrk.no/",
		Podcasts:    make([]ManifestPodcast, 0, len(podcasts)),
		Failures:    make([]ManifestFailure, 0, len(opts.Failures)),
	}

	written := make(map[string]bool, len(podcasts))
	var latestOverall time.Time

	for _, p := range podcasts {
		if !feed.SafeFilename(p.ID) {
			log.Warn("skipping podcast with unsafe id", "podcast", p.ID)
			continue
		}

		episodes, err := st.EpisodesForPodcast(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		// A podcast with no playable episodes would produce an empty feed
		// that no client can subscribe to usefully.
		if len(episodes) == 0 {
			log.Debug("skipping podcast with no stored episodes", "podcast", p.ID)
			continue
		}

		filename := p.ID + ".xml"
		feedURL := joinURL(opts.BaseURL, "feeds/"+filename)

		rss := feed.Build(p, episodes, feed.Options{
			SelfLink:  feedURL,
			Generator: opts.Generator,
		})
		if err := feed.WriteFile(filepath.Join(feedsDir, filename), rss); err != nil {
			return nil, err
		}
		written[filename] = true

		manifest.Podcasts = append(manifest.Podcasts, ManifestPodcast{
			ID:            p.ID,
			Title:         p.Title,
			Description:   p.Description,
			Category:      p.CategoryName,
			ImageURL:      p.ImageURL,
			EpisodeCount:  len(episodes),
			FeedURL:       feedURL,
			NRKURL:        feed.PodcastPageURL(p.ID),
			LatestEpisode: episodes[0].PubDate.UTC().Format(time.RFC3339),
		})
		manifest.EpisodeCount += len(episodes)

		// Episodes come back newest-first, so the head is this podcast's latest.
		if latest := episodes[0].PubDate; latest.After(latestOverall) {
			latestOverall = latest
		}
	}

	manifest.PodcastCount = len(manifest.Podcasts)
	if !latestOverall.IsZero() {
		manifest.LatestEpisode = latestOverall.UTC().Format(time.RFC3339)
	}

	for id, msg := range opts.Failures {
		manifest.Failures = append(manifest.Failures, ManifestFailure{ID: id, Error: msg})
	}
	sort.Slice(manifest.Failures, func(i, j int) bool {
		return manifest.Failures[i].ID < manifest.Failures[j].ID
	})

	if err := pruneStaleFeeds(feedsDir, written, log); err != nil {
		return nil, err
	}
	if err := writeManifest(filepath.Join(opts.OutDir, "feeds.json"), manifest); err != nil {
		return nil, err
	}
	if err := writeIndex(filepath.Join(opts.OutDir, "index.html"), manifest); err != nil {
		return nil, err
	}
	if err := writeNoJekyll(opts.OutDir); err != nil {
		return nil, err
	}

	log.Info("site exported",
		"dir", opts.OutDir,
		"podcasts", manifest.PodcastCount,
		"episodes", manifest.EpisodeCount,
		"failures", len(manifest.Failures))
	return manifest, nil
}

// joinURL builds a feed URL, staying relative when no base URL is configured.
func joinURL(base, rel string) string {
	if base == "" {
		return rel
	}
	return strings.TrimRight(base, "/") + "/" + rel
}

// pruneStaleFeeds removes feed files for podcasts that are no longer in the
// store, so a delisted podcast does not linger on the site forever.
func pruneStaleFeeds(feedsDir string, written map[string]bool, log *slog.Logger) error {
	entries, err := os.ReadDir(feedsDir)
	if err != nil {
		return fmt.Errorf("site: reading %s: %w", feedsDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".xml") || written[name] {
			continue
		}
		if err := os.Remove(filepath.Join(feedsDir, name)); err != nil {
			return fmt.Errorf("site: removing stale feed %s: %w", name, err)
		}
		log.Info("removed stale feed", "file", name)
	}
	return nil
}

func writeManifest(path string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("site: encoding manifest: %w", err)
	}
	b = append(b, '\n')

	// generated_at changes on every run by definition. If it is the only
	// difference, the manifest is left untouched so a run that found nothing
	// new produces no diff to commit.
	if same, err := manifestDiffersOnlyByTimestamp(path, m, b); err == nil && same {
		return nil
	}
	return writeFileAtomic(path, b)
}

// manifestDiffersOnlyByTimestamp reports whether the manifest already on disk
// is identical to the new one except for generated_at.
func manifestDiffersOnlyByTimestamp(path string, fresh *Manifest, freshBytes []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	var old Manifest
	if err := json.Unmarshal(existing, &old); err != nil {
		return false, err
	}
	old.GeneratedAt = fresh.GeneratedAt

	normalised, err := json.MarshalIndent(&old, "", "  ")
	if err != nil {
		return false, err
	}
	return string(append(normalised, '\n')) == string(freshBytes), nil
}

func writeIndex(path string, m *Manifest) error {
	tmpl, err := template.New("index").Parse(indexTemplate)
	if err != nil {
		return fmt.Errorf("site: parsing index template: %w", err)
	}

	// The list is rendered server-side and filtered client-side, so the page
	// still works with JavaScript disabled and needs no fetch to load.
	var buf strings.Builder
	if err := tmpl.Execute(&buf, m); err != nil {
		return fmt.Errorf("site: rendering index: %w", err)
	}
	return writeFileAtomic(path, []byte(buf.String()))
}

// writeNoJekyll stops GitHub Pages running the output through Jekyll, which
// would otherwise ignore files and directories beginning with an underscore.
func writeNoJekyll(dir string) error {
	return writeFileAtomic(filepath.Join(dir, ".nojekyll"), nil)
}

// writeFileAtomic writes via a temporary file in the same directory so an
// interrupted run cannot leave a half-written file behind. Unchanged content
// is not rewritten, keeping the committed tree free of no-op diffs.
func writeFileAtomic(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("site: creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("site: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("site: closing temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("site: writing %s: %w", path, err)
	}
	return nil
}
