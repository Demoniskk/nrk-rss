// Package scrape drives the NRK API and the state store: it decides which
// episodes still need fetching and does the fetching, politely and in
// parallel, without letting one bad podcast take down a run.
package scrape

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Demoniskk/nrk-rss/internal/nrkapi"
	"github.com/Demoniskk/nrk-rss/internal/store"
)

// Mode selects how far back through a podcast's catalogue a run reads.
type Mode int

const (
	// ModeFull pages through a podcast's entire back catalogue. Used by
	// scrape-all.
	ModeFull Mode = iota
	// ModeIncremental pages newest-first and stops at the first episode
	// already in the store. Used by update.
	ModeIncremental
)

func (m Mode) String() string {
	if m == ModeFull {
		return "full"
	}
	return "incremental"
}

// Options configures a run.
type Options struct {
	Mode Mode
	// Concurrency is how many podcasts are worked on at once.
	Concurrency int
	// EpisodeConcurrency is how many episodes are fetched at once within a
	// single podcast. Total in-flight requests stay under
	// Concurrency * EpisodeConcurrency.
	EpisodeConcurrency int
	// PageSize is the episode-list page size requested from NRK.
	PageSize int
	// MaxPagesPerPodcast caps how many episode-list pages an incremental run
	// reads before giving up on finding a known episode. Zero means no cap.
	// It does not apply to ModeFull, nor to podcasts with no stored episodes.
	MaxPagesPerPodcast int
	// Force re-fetches every episode even if it is already stored. Combined
	// with ModeFull this rebuilds the state from scratch.
	Force bool
	// Rich adds one API call per new episode to pick up chapter markers and
	// contributor credits, which the episode list does not carry.
	Rich bool
	// Only, when non-empty, restricts the run to these podcast IDs. Useful for
	// testing and for repairing a single feed.
	Only []string
}

// Defaults fills in zero-valued options with their defaults.
func (o *Options) Defaults() {
	if o.Concurrency < 1 {
		o.Concurrency = 5
	}
	if o.EpisodeConcurrency < 1 {
		o.EpisodeConcurrency = 3
	}
	if o.PageSize < 1 {
		o.PageSize = 50
	}
}

// Result summarises a run.
type Result struct {
	PodcastsSeen      int
	PodcastsSucceeded int
	PodcastsFailed    int
	EpisodesAdded     int
	EpisodesSkipped   int // discovered but unplayable
	// Failures maps podcast ID to the error that ended its scrape.
	Failures map[string]string
}

// Scraper fetches podcasts into the state store.
type Scraper struct {
	Client *nrkapi.Client
	Store  *store.Store
	Logger *slog.Logger
}

// New returns a Scraper.
func New(c *nrkapi.Client, s *store.Store, logger *slog.Logger) *Scraper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scraper{Client: c, Store: s, Logger: logger}
}

// Run discovers the podcast catalogue and scrapes each podcast according to
// opts. A podcast that fails is recorded and the run continues; Run only
// returns an error if the catalogue itself could not be read.
func (s *Scraper) Run(ctx context.Context, opts Options) (*Result, error) {
	opts.Defaults()

	catalogue, err := s.Client.ListPodcasts(ctx)
	if err != nil {
		return nil, fmt.Errorf("scrape: listing podcast catalogue: %w", err)
	}
	if len(opts.Only) > 0 {
		catalogue = filterByID(catalogue, opts.Only)
	}
	s.Logger.Info("podcast catalogue loaded", "podcasts", len(catalogue), "mode", opts.Mode.String())

	res := &Result{PodcastsSeen: len(catalogue), Failures: map[string]string{}}
	var (
		mu      sync.Mutex
		runOver bool // a worker stopped because the run is ending
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)

	for _, entry := range catalogue {
		g.Go(func() error {
			added, skipped, err := s.scrapePodcast(gctx, entry, opts)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// A cancelled or timed-out run is not a per-podcast failure;
				// it is the whole run being stopped, and is reported by Run's
				// caller. The error is checked as well as the context, because
				// the rate limiter gives up on a request slightly before the
				// deadline it would have overshot actually arrives.
				if gctx.Err() != nil || isRunOver(err) {
					runOver = true
					return nil
				}
				res.PodcastsFailed++
				res.Failures[entry.ID] = err.Error()
				s.Logger.Error("podcast failed", "podcast", entry.ID, "error", err)
				return nil
			}
			res.PodcastsSucceeded++
			res.EpisodesAdded += added
			res.EpisodesSkipped += skipped
			return nil
		})
	}

	// The goroutines never return an error, so Wait only surfaces panics.
	if err := g.Wait(); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// Workers can bail out fractionally before the deadline formally passes,
	// leaving ctx.Err() nil even though the run really did run out of time.
	if runOver {
		return res, context.DeadlineExceeded
	}
	return res, nil
}

// isRunOver reports whether an error means the run itself is finishing —
// cancelled by a signal, or out of its time budget — rather than this
// particular podcast being broken.
func isRunOver(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func filterByID(catalogue []nrkapi.SearchSeries, only []string) []nrkapi.SearchSeries {
	want := make(map[string]bool, len(only))
	for _, id := range only {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	out := catalogue[:0:0]
	for _, e := range catalogue {
		if want[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// scrapePodcast fetches one podcast's metadata and any episodes not already
// stored, returning how many episodes were added and how many were discovered
// but skipped as unplayable.
func (s *Scraper) scrapePodcast(ctx context.Context, entry nrkapi.SearchSeries, opts Options) (added, skipped int, err error) {
	log := s.Logger.With("podcast", entry.ID)

	meta, err := s.Client.GetPodcast(ctx, entry.ID)
	if err != nil {
		return 0, 0, fmt.Errorf("fetching podcast metadata: %w", err)
	}

	p := podcastFromAPI(entry, meta)
	if err := s.Store.UpsertPodcast(ctx, p); err != nil {
		return 0, 0, err
	}

	known, err := s.Store.KnownEpisodeIDs(ctx, entry.ID)
	if err != nil {
		return 0, 0, err
	}
	if opts.Force {
		// Everything is treated as new, so nothing is reused from the store.
		known = map[string]bool{}
	}

	fresh, err := s.discoverNewEpisodes(ctx, entry.ID, known, opts)
	if err != nil {
		return 0, 0, err
	}
	if len(fresh) == 0 {
		log.Debug("no new episodes")
		return 0, 0, nil
	}
	log.Info("new episodes discovered", "count", len(fresh))

	episodes, skipped, err := s.fetchEpisodes(ctx, entry.ID, fresh, opts)
	if err != nil {
		return 0, 0, err
	}
	if err := s.Store.UpsertEpisodes(ctx, episodes); err != nil {
		return 0, 0, err
	}

	log.Info("podcast scraped", "added", len(episodes), "skipped", skipped)
	return len(episodes), skipped, nil
}

func podcastFromAPI(entry nrkapi.SearchSeries, meta *nrkapi.PodcastResponse) store.Podcast {
	series := meta.Series

	title := series.Titles.Title
	if title == "" {
		title = entry.Title
	}

	// Square artwork is what podcast clients want; fall back through the
	// landscape image and then the search result's own renditions.
	image := series.SquareImage.Widest()
	if image == "" {
		image = series.Image.Widest()
	}
	if image == "" {
		image = entry.SquareImages.Widest()
	}
	if image == "" {
		image = entry.Images.Widest()
	}

	return store.Podcast{
		ID:           entry.ID,
		Title:        title,
		Description:  series.Titles.Subtitle,
		CategoryID:   series.Category.ID,
		CategoryName: series.Category.Name,
		ImageURL:     image,
		LastScraped:  time.Now().UTC(),
	}
}

// discoverNewEpisodes pages through a podcast's episode list and returns the
// episodes that are not already stored.
//
// In incremental mode it stops at the first already-known episode, since the
// list is newest-first and everything past that point is already stored. A
// podcast with nothing stored yet is always read in full regardless of mode:
// it is new to us, and capping it would silently leave a permanent hole in
// its feed that a later incremental run would never fill.
func (s *Scraper) discoverNewEpisodes(ctx context.Context, podcastID string, known map[string]bool, opts Options) ([]nrkapi.Episode, error) {
	incremental := opts.Mode == ModeIncremental && len(known) > 0
	maxPages := 0
	if incremental {
		maxPages = opts.MaxPagesPerPodcast
	}

	var (
		fresh []nrkapi.Episode
		seen  = make(map[string]bool)
	)

	for page := 1; ; page++ {
		if maxPages > 0 && page > maxPages {
			s.Logger.Warn("page cap reached before finding a known episode",
				"podcast", podcastID, "max_pages", maxPages)
			break
		}

		resp, err := s.Client.EpisodePage(ctx, podcastID, page, opts.PageSize)
		if err != nil {
			if errors.Is(err, nrkapi.ErrNotFound) && page > 1 {
				// Some podcasts 404 one page past the end instead of simply
				// omitting the next link.
				break
			}
			return nil, fmt.Errorf("fetching episode page %d: %w", page, err)
		}

		hitKnown := false
		for _, ep := range resp.Embedded.Episodes {
			if ep.EpisodeID == "" || seen[ep.EpisodeID] {
				continue
			}
			seen[ep.EpisodeID] = true

			if known[ep.EpisodeID] {
				hitKnown = true
				if incremental {
					break
				}
				continue
			}
			fresh = append(fresh, ep)
		}

		if incremental && hitKnown {
			break
		}
		if !resp.HasNext() {
			break
		}
	}

	return fresh, nil
}

// fetchEpisodes turns discovered episodes into store records, doing the
// manifest lookup and the enclosure HEAD for each. Episodes NRK will not serve
// are skipped rather than failing the podcast.
func (s *Scraper) fetchEpisodes(ctx context.Context, podcastID string, fresh []nrkapi.Episode, opts Options) ([]store.Episode, int, error) {
	var (
		mu      sync.Mutex
		out     = make([]store.Episode, 0, len(fresh))
		skipped int
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.EpisodeConcurrency)

	for _, ep := range fresh {
		g.Go(func() error {
			rec, err := s.fetchEpisode(gctx, podcastID, ep, opts)
			if err != nil {
				// Unplayable episodes are routine: geo-blocking and expiry
				// both surface here. Anything else fails the podcast so the
				// run reports it rather than publishing a feed with a hole.
				if errors.Is(err, nrkapi.ErrNotPlayable) || errors.Is(err, nrkapi.ErrNotFound) {
					s.Logger.Info("skipping episode",
						"podcast", podcastID, "episode", ep.EpisodeID, "reason", err)
					mu.Lock()
					skipped++
					mu.Unlock()
					return nil
				}
				return fmt.Errorf("episode %s: %w", ep.EpisodeID, err)
			}
			mu.Lock()
			out = append(out, *rec)
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, 0, err
	}

	// Deterministic order keeps the committed database and the generated
	// feeds byte-stable between runs that fetched the same episodes.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PubDate.Equal(out[j].PubDate) {
			return out[i].PubDate.After(out[j].PubDate)
		}
		return out[i].EpisodeID < out[j].EpisodeID
	})
	return out, skipped, nil
}

// fetchEpisode performs the per-episode work: the playback manifest, the
// enclosure HEAD, and — only when Rich is set — the episode-detail call that
// carries chapter markers and credits.
func (s *Scraper) fetchEpisode(ctx context.Context, podcastID string, ep nrkapi.Episode, opts Options) (*store.Episode, error) {
	if opts.Rich {
		detail, err := s.Client.GetEpisode(ctx, podcastID, ep.EpisodeID)
		if err != nil {
			// The extras are a bonus; losing them is not worth losing the
			// episode, so fall through with what the list gave us.
			s.Logger.Warn("episode detail unavailable",
				"podcast", podcastID, "episode", ep.EpisodeID, "error", err)
		} else {
			ep = *detail
		}
	}

	asset, err := s.Client.AudioAsset(ctx, ep.EpisodeID)
	if err != nil {
		return nil, err
	}

	info, err := s.Client.HeadAsset(ctx, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("heading enclosure: %w", err)
	}

	pubDate, err := ep.PublishedAt()
	if err != nil {
		return nil, err
	}

	seasonID, seasonName := ep.Season()
	rec := &store.Episode{
		PodcastID:       podcastID,
		EpisodeID:       ep.EpisodeID,
		Title:           ep.Titles.Title,
		Description:     ep.Titles.Subtitle,
		PubDate:         pubDate,
		DurationSeconds: ep.DurationSeconds(),
		EnclosureURL:    asset.URL,
		EnclosureType:   enclosureType(info.Type, asset.MimeType),
		EnclosureLength: info.Length,
		ImageURL:        ep.SquareImage.Widest(),
		SeasonID:        seasonID,
		SeasonName:      seasonName,
		FetchedAt:       time.Now().UTC(),
	}
	if rec.ImageURL == "" {
		rec.ImageURL = ep.Image.Widest()
	}

	for _, ip := range ep.IndexPoints {
		if ip.Title == "" {
			continue
		}
		rec.IndexPoints = append(rec.IndexPoints, store.IndexPoint{
			Title:        ip.Title,
			StartSeconds: ip.StartSeconds(),
		})
	}
	for _, c := range ep.Contributors {
		if len(c.Name) == 0 {
			continue
		}
		rec.Contributors = append(rec.Contributors, store.Contributor{Role: c.Role, Name: c.Name})
	}

	return rec, nil
}

// enclosureType prefers the content type the CDN actually served, falling back
// to the manifest's mimeType. NRK reports "audio/mp3" in manifests, which is
// not a registered type; the HEAD response gives the correct "audio/mpeg".
func enclosureType(headType, manifestType string) string {
	if t := strings.TrimSpace(strings.Split(headType, ";")[0]); t != "" && t != "application/octet-stream" {
		return t
	}
	switch strings.ToLower(strings.TrimSpace(manifestType)) {
	case "audio/mp3", "":
		return "audio/mpeg"
	default:
		return manifestType
	}
}
