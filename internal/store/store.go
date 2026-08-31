// Package store is the persistent state for the scraper: which episodes have
// already been fully fetched, and enough data about each to re-render its RSS
// item without touching the network again.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so CI needs no cgo
)

// Store wraps the SQLite database holding scrape state.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS episodes (
    podcast_id       TEXT NOT NULL,
    episode_id       TEXT NOT NULL,
    title            TEXT NOT NULL,
    description      TEXT,
    pub_date         TEXT NOT NULL,
    duration_seconds INTEGER NOT NULL,
    enclosure_url    TEXT NOT NULL,
    enclosure_type   TEXT NOT NULL,
    enclosure_length INTEGER NOT NULL,
    image_url        TEXT,
    season_id        TEXT,
    season_name      TEXT,
    index_points     TEXT,
    contributors     TEXT,
    fetched_at       TEXT NOT NULL,
    PRIMARY KEY (podcast_id, episode_id)
);

CREATE INDEX IF NOT EXISTS idx_episodes_podcast_date
    ON episodes (podcast_id, pub_date DESC);

CREATE TABLE IF NOT EXISTS podcasts (
    podcast_id    TEXT PRIMARY KEY,
    title         TEXT NOT NULL,
    description   TEXT,
    category_id   TEXT,
    category_name TEXT,
    image_url     TEXT,
    last_scraped  TEXT NOT NULL
);
`

// Open opens (creating if needed) the state database at path and applies the
// schema. The parent directory is created if it does not exist.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: creating %s: %w", dir, err)
		}
	}

	// WAL keeps concurrent readers from blocking the writer; busy_timeout
	// covers the brief contention when many podcast workers commit at once.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}

	// SQLite takes a single write lock for the whole file. Serialising access
	// here is simpler and no slower than letting goroutines collide on it.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: applying schema to %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Podcast is a podcast's feed-level metadata as stored.
type Podcast struct {
	ID           string
	Title        string
	Description  string
	CategoryID   string
	CategoryName string
	ImageURL     string
	LastScraped  time.Time
}

// IndexPoint is a stored chapter marker.
type IndexPoint struct {
	Title        string `json:"title"`
	StartSeconds int    `json:"start_seconds"`
}

// Contributor is a stored credit.
type Contributor struct {
	Role string   `json:"role"`
	Name []string `json:"name"`
}

// Episode is everything needed to render one RSS item offline.
type Episode struct {
	PodcastID       string
	EpisodeID       string
	Title           string
	Description     string
	PubDate         time.Time
	DurationSeconds int
	EnclosureURL    string
	EnclosureType   string
	EnclosureLength int64
	ImageURL        string
	SeasonID        string
	SeasonName      string
	IndexPoints     []IndexPoint
	Contributors    []Contributor
	FetchedAt       time.Time
}

// UpsertPodcast inserts or updates a podcast's metadata.
//
// The update is conditional on something having actually changed. last_scraped
// therefore records when this podcast's metadata last changed, not when it was
// last looked at — which is what keeps the committed database byte-identical
// across runs that found nothing new, so the daily job has no empty commit to
// make.
func (s *Store) UpsertPodcast(ctx context.Context, p Podcast) error {
	const q = `
INSERT INTO podcasts (podcast_id, title, description, category_id, category_name, image_url, last_scraped)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(podcast_id) DO UPDATE SET
    title         = excluded.title,
    description   = excluded.description,
    category_id   = excluded.category_id,
    category_name = excluded.category_name,
    image_url     = excluded.image_url,
    last_scraped  = excluded.last_scraped
WHERE podcasts.title         IS NOT excluded.title
   OR podcasts.description   IS NOT excluded.description
   OR podcasts.category_id   IS NOT excluded.category_id
   OR podcasts.category_name IS NOT excluded.category_name
   OR podcasts.image_url     IS NOT excluded.image_url;`

	_, err := s.db.ExecContext(ctx, q,
		p.ID, p.Title, p.Description, p.CategoryID, p.CategoryName, p.ImageURL,
		p.LastScraped.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store: upserting podcast %s: %w", p.ID, err)
	}
	return nil
}

// UpsertEpisodes writes a batch of episodes in a single transaction.
func (s *Store) UpsertEpisodes(ctx context.Context, eps []Episode) error {
	if len(eps) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once the tx is committed

	const q = `
INSERT INTO episodes (
    podcast_id, episode_id, title, description, pub_date, duration_seconds,
    enclosure_url, enclosure_type, enclosure_length, image_url,
    season_id, season_name, index_points, contributors, fetched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(podcast_id, episode_id) DO UPDATE SET
    title            = excluded.title,
    description      = excluded.description,
    pub_date         = excluded.pub_date,
    duration_seconds = excluded.duration_seconds,
    enclosure_url    = excluded.enclosure_url,
    enclosure_type   = excluded.enclosure_type,
    enclosure_length = excluded.enclosure_length,
    image_url        = excluded.image_url,
    season_id        = excluded.season_id,
    season_name      = excluded.season_name,
    index_points     = excluded.index_points,
    contributors     = excluded.contributors,
    fetched_at       = excluded.fetched_at;`

	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("store: preparing episode upsert: %w", err)
	}
	defer stmt.Close()

	for _, e := range eps {
		points, err := marshalJSON(e.IndexPoints)
		if err != nil {
			return fmt.Errorf("store: encoding index points for %s: %w", e.EpisodeID, err)
		}
		contribs, err := marshalJSON(e.Contributors)
		if err != nil {
			return fmt.Errorf("store: encoding contributors for %s: %w", e.EpisodeID, err)
		}

		_, err = stmt.ExecContext(ctx,
			e.PodcastID, e.EpisodeID, e.Title, e.Description,
			e.PubDate.Format(time.RFC3339), e.DurationSeconds,
			e.EnclosureURL, e.EnclosureType, e.EnclosureLength, e.ImageURL,
			e.SeasonID, e.SeasonName, points, contribs,
			e.FetchedAt.UTC().Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("store: upserting episode %s/%s: %w", e.PodcastID, e.EpisodeID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing episodes: %w", err)
	}
	return nil
}

// KnownEpisodeIDs returns the set of episode IDs already fully fetched for a
// podcast. This is what update diffs against to decide what is new.
func (s *Store) KnownEpisodeIDs(ctx context.Context, podcastID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT episode_id FROM episodes WHERE podcast_id = ?`, podcastID)
	if err != nil {
		return nil, fmt.Errorf("store: listing known episodes for %s: %w", podcastID, err)
	}
	defer rows.Close()

	known := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning episode id: %w", err)
		}
		known[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading episode ids for %s: %w", podcastID, err)
	}
	return known, nil
}

// EpisodesForPodcast returns a podcast's stored episodes, newest first.
func (s *Store) EpisodesForPodcast(ctx context.Context, podcastID string) ([]Episode, error) {
	const q = `
SELECT podcast_id, episode_id, title, COALESCE(description, ''), pub_date,
       duration_seconds, enclosure_url, enclosure_type, enclosure_length,
       COALESCE(image_url, ''), COALESCE(season_id, ''), COALESCE(season_name, ''),
       COALESCE(index_points, ''), COALESCE(contributors, ''), fetched_at
FROM episodes
WHERE podcast_id = ?
ORDER BY pub_date DESC;`

	rows, err := s.db.QueryContext(ctx, q, podcastID)
	if err != nil {
		return nil, fmt.Errorf("store: loading episodes for %s: %w", podcastID, err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		var (
			e                         Episode
			pubDate, fetchedAt        string
			indexPoints, contributors string
		)
		if err := rows.Scan(
			&e.PodcastID, &e.EpisodeID, &e.Title, &e.Description, &pubDate,
			&e.DurationSeconds, &e.EnclosureURL, &e.EnclosureType, &e.EnclosureLength,
			&e.ImageURL, &e.SeasonID, &e.SeasonName, &indexPoints, &contributors, &fetchedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scanning episode: %w", err)
		}

		if e.PubDate, err = time.Parse(time.RFC3339, pubDate); err != nil {
			return nil, fmt.Errorf("store: parsing pub_date for %s: %w", e.EpisodeID, err)
		}
		// A bad fetched_at is not worth failing a whole feed over.
		e.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAt)

		if err := unmarshalJSON(indexPoints, &e.IndexPoints); err != nil {
			return nil, fmt.Errorf("store: decoding index points for %s: %w", e.EpisodeID, err)
		}
		if err := unmarshalJSON(contributors, &e.Contributors); err != nil {
			return nil, fmt.Errorf("store: decoding contributors for %s: %w", e.EpisodeID, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading episodes for %s: %w", podcastID, err)
	}
	return out, nil
}

// AllPodcasts returns every stored podcast, ordered by title.
func (s *Store) AllPodcasts(ctx context.Context) ([]Podcast, error) {
	const q = `
SELECT podcast_id, title, COALESCE(description, ''), COALESCE(category_id, ''),
       COALESCE(category_name, ''), COALESCE(image_url, ''), last_scraped
FROM podcasts
ORDER BY title COLLATE NOCASE;`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: listing podcasts: %w", err)
	}
	defer rows.Close()

	var out []Podcast
	for rows.Next() {
		var (
			p           Podcast
			lastScraped string
		)
		if err := rows.Scan(&p.ID, &p.Title, &p.Description, &p.CategoryID,
			&p.CategoryName, &p.ImageURL, &lastScraped); err != nil {
			return nil, fmt.Errorf("store: scanning podcast: %w", err)
		}
		p.LastScraped, _ = time.Parse(time.RFC3339, lastScraped)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading podcasts: %w", err)
	}
	return out, nil
}

// EpisodeCounts returns the number of stored episodes per podcast ID.
func (s *Store) EpisodeCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT podcast_id, COUNT(*) FROM episodes GROUP BY podcast_id`)
	if err != nil {
		return nil, fmt.Errorf("store: counting episodes: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: scanning episode count: %w", err)
		}
		counts[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading episode counts: %w", err)
	}
	return counts, nil
}

// marshalJSON encodes a slice, returning "" for empty input so the column
// stays small and readable in the committed database.
func marshalJSON[T any](v []T) (string, error) {
	if len(v) == 0 {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalJSON[T any](s string, out *[]T) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), out)
}
