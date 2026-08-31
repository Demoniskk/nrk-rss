// Package nrkapi is a minimal client for the public, unauthenticated JSON
// endpoints behind NRK Radio (psapi.nrk.no).
package nrkapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	// DefaultBaseURL is NRK's public programme-service API.
	DefaultBaseURL = "https://psapi.nrk.no"
	// acceptHeader pins the API version these types were written against.
	acceptHeader = "application/json;api-version=3.5"
	// DefaultUserAgent identifies this tool to NRK.
	DefaultUserAgent = "nrk-podcast-rss/1.0 (+https://github.com/Demoniskk/nrk-rss)"
	// DefaultRate is the sustained request rate, in requests per second, for
	// the psapi.nrk.no API.
	//
	// Measured against the live API: 5 req/s over 50 requests and 3 req/s over
	// 90 requests both came back clean, while an unpaced pool of 20 workers
	// (well over 100 req/s) was rate-limited within seconds. This default sits
	// just under the measured ceiling.
	DefaultRate = 4.0
	// DefaultBurst is how many requests may be sent back-to-back before the
	// rate limit applies. Kept small: bursts are what trip NRK's limiter.
	DefaultBurst = 2
	// DefaultAssetRate paces HEAD requests for audio files. Those go to the
	// podcast CDN rather than the API, so they get their own budget.
	DefaultAssetRate = 8.0
	// MaxPageSize is the largest episode-list page NRK will serve. Measured
	// against the live API: 50 returns 200, and 51 and above return 400. It is
	// also the endpoint's default.
	MaxPageSize = 50
)

// Client talks to the NRK programme-service API. It is safe for concurrent
// use by multiple goroutines.
type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client
	Logger    *slog.Logger

	// MaxAttempts is the total number of tries for a request that fails with
	// a retryable error (429, 5xx, transport error). Defaults to 5.
	MaxAttempts int
	// BaseBackoff is the delay before the second attempt; it doubles for each
	// subsequent attempt. Defaults to one second.
	BaseBackoff time.Duration
	// MaxRetryWait caps how long a 429 may pause the client, including a
	// Retry-After the server asked for. NRK asks for 600 seconds and, as
	// measured, really does stay rate-limited for about that long if requests
	// keep arriving — so the cap defaults to ten minutes rather than
	// second-guessing the server.
	MaxRetryWait time.Duration

	// limiter paces API requests. NRK rate-limits on total request rate, not
	// per connection, so one limiter is shared by every worker.
	limiter *rate.Limiter
	// assetLimiter paces HEAD requests to the audio CDN, which is a different
	// host with its own budget.
	assetLimiter *rate.Limiter
	// throttle pauses every worker after any one of them is rate-limited.
	throttle throttle
}

// New returns a Client with sensible defaults.
func New(logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		BaseURL:   DefaultBaseURL,
		UserAgent: DefaultUserAgent,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
		},
		Logger:       logger,
		MaxAttempts:  5,
		BaseBackoff:  time.Second,
		MaxRetryWait: 10 * time.Minute,
		limiter:      rate.NewLimiter(rate.Limit(DefaultRate), DefaultBurst),
		assetLimiter: rate.NewLimiter(rate.Limit(DefaultAssetRate), DefaultBurst),
	}
}

// SetRate changes the sustained API request rate, in requests per second, and
// scales the audio-CDN budget with it. A rate of zero or less disables pacing,
// which is only sensible against a test server.
func (c *Client) SetRate(perSecond float64, burst int) {
	if perSecond <= 0 {
		c.limiter, c.assetLimiter = nil, nil
		return
	}
	if burst < 1 {
		burst = 1
	}
	c.limiter = rate.NewLimiter(rate.Limit(perSecond), burst)
	c.assetLimiter = rate.NewLimiter(rate.Limit(perSecond*(DefaultAssetRate/DefaultRate)), burst)
}

// ErrNotFound is returned when NRK answers a request with 404. Callers use it
// to distinguish "this podcast is gone" from "this request failed".
var ErrNotFound = errors.New("nrkapi: not found")

// ErrNotPlayable is returned for episodes NRK will not serve audio for, which
// is normal for geo-blocked or expired items.
var ErrNotPlayable = errors.New("nrkapi: episode not playable")

// statusError carries an HTTP failure response.
type statusError struct {
	Status int
	URL    string
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("nrkapi: GET %s: unexpected status %d: %s", e.URL, e.Status, e.Body)
}

func (c *Client) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return 5
}

func (c *Client) maxRetryWait() time.Duration {
	if c.MaxRetryWait > 0 {
		return c.MaxRetryWait
	}
	return 10 * time.Minute
}

// gate blocks until this request is allowed to go out: first any global pause
// left by a 429 anywhere, then the steady-state rate limit for the host being
// addressed.
func (c *Client) gate(ctx context.Context, rawURL string) error {
	if err := c.throttle.wait(ctx); err != nil {
		return err
	}

	limiter := c.limiter
	if !strings.HasPrefix(rawURL, c.base()) {
		// Not the API: this is a HEAD against the audio CDN.
		limiter = c.assetLimiter
	}
	if limiter == nil {
		return nil
	}

	if err := limiter.Wait(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// The limiter refuses as soon as it can see the wait would outlast the
		// context's deadline, which is before the deadline actually arrives.
		// Reporting that as the context error lets callers recognise it as the
		// run ending rather than as a request failing.
		return fmt.Errorf("nrkapi: rate limiter: %w", context.DeadlineExceeded)
	}
	return nil
}

func (c *Client) baseBackoff() time.Duration {
	if c.BaseBackoff > 0 {
		return c.BaseBackoff
	}
	return time.Second
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return DefaultUserAgent
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

// do performs an HTTP request with the API headers set, retrying on 429, 5xx
// and transport errors with jittered exponential backoff.
//
// A 429 additionally pauses every worker for the Retry-After interval NRK
// asked for. The rate limiter is applied after that pause, so the workers
// released by it do not all fire at once.
func (c *Client) do(ctx context.Context, method, rawURL string) (*http.Response, error) {
	attempts := c.maxAttempts()
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			// Exponential backoff with jitter, so a batch of workers that were
			// rate-limited together does not retry in lockstep.
			delay := time.Duration(math.Pow(2, float64(attempt-2))) * c.baseBackoff()
			delay += time.Duration(rand.Int64N(int64(c.baseBackoff())))
			if delay > c.maxRetryWait() {
				delay = c.maxRetryWait()
			}
			c.Logger.Debug("retrying request",
				"url", rawURL, "attempt", attempt, "delay", delay, "prev_error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		// Wait out any global pause and the steady-state rate limit before
		// putting another request on the wire.
		if err := c.gate(ctx, rawURL); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("nrkapi: building request for %s: %w", rawURL, err)
		}
		req.Header.Set("Accept", acceptHeader)
		req.Header.Set("User-Agent", c.userAgent())

		resp, err := c.HTTP.Do(req)
		if err != nil {
			// A cancelled context is the caller giving up, not a flaky server.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("nrkapi: %s %s: %w", method, rawURL, err)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrNotFound, rawURL)
		case resp.StatusCode == http.StatusTooManyRequests:
			// Pause every worker, not just this one. NRK's limiter keeps the
			// penalty alive while requests continue to arrive, so backing off
			// a single goroutine while nineteen others keep going never
			// recovers.
			wait := parseRetryAfter(resp.Header.Get("Retry-After"), c.maxRetryWait())
			if wait <= 0 {
				wait = c.maxRetryWait()
			}
			until := c.throttle.penalize(wait)
			c.Logger.Warn("rate limited by NRK; pausing all requests",
				"url", rawURL, "wait", wait, "until", until.Format(time.RFC3339))

			lastErr = &statusError{Status: resp.StatusCode, URL: rawURL, Body: peekBody(resp)}
			resp.Body.Close()
			continue

		case resp.StatusCode >= 500:
			lastErr = &statusError{Status: resp.StatusCode, URL: rawURL, Body: peekBody(resp)}
			resp.Body.Close()
			continue
		case resp.StatusCode >= 400:
			body := peekBody(resp)
			resp.Body.Close()
			return nil, &statusError{Status: resp.StatusCode, URL: rawURL, Body: body}
		default:
			return resp, nil
		}
	}
	return nil, fmt.Errorf("nrkapi: giving up on %s after %d attempts: %w", rawURL, attempts, lastErr)
}

// parseRetryAfter understands the delay-seconds form of Retry-After, which is
// what NRK sends (it asks for 600). The HTTP-date form is not used by this API
// and is ignored. The wait is capped so a misbehaving header cannot stall a
// run indefinitely.
func parseRetryAfter(v string, max time.Duration) time.Duration {
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs < 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if max > 0 && d > max {
		d = max
	}
	return d
}

// peekBody reads a bounded prefix of an error response for logging.
func peekBody(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return ""
	}
	return string(b)
}

// getJSON fetches rawURL and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("nrkapi: decoding %s: %w", rawURL, err)
	}
	return nil
}

// ListPodcasts returns every series in NRK's podcast category, filtered to
// entries that are podcasts in their own right. Entries of type
// "customSeason", "series" and "singleProgram" are skipped: they are slices of
// or pointers to other series rather than separate podcasts.
func (c *Client) ListPodcasts(ctx context.Context) ([]SearchSeries, error) {
	u := c.base() + "/radio/search/categories/podcast?take=1000"
	var out SearchResponse
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}

	podcasts := make([]SearchSeries, 0, len(out.Series))
	seen := make(map[string]bool, len(out.Series))
	for _, s := range out.Series {
		if s.Type != "podcast" || s.ID == "" || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		podcasts = append(podcasts, s)
	}
	return podcasts, nil
}

// GetPodcast returns a single podcast's series metadata.
func (c *Client) GetPodcast(ctx context.Context, podcastID string) (*PodcastResponse, error) {
	u := c.base() + "/radio/catalog/podcast/" + url.PathEscape(podcastID)
	var out PodcastResponse
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EpisodePage fetches one page of a podcast's episodes. Pages are 1-based and
// ordered newest-first.
//
// pageSize is clamped to MaxPageSize: NRK rejects anything larger with a 400,
// which is not retryable and would otherwise fail every podcast in the run.
func (c *Client) EpisodePage(ctx context.Context, podcastID string, page, pageSize int) (*EpisodesResponse, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}
	u := fmt.Sprintf("%s/radio/catalog/podcast/%s/episodes?pageSize=%d&page=%d",
		c.base(), url.PathEscape(podcastID), pageSize, page)
	var out EpisodesResponse
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEpisode fetches a single episode by ID. Unlike the episode list, this
// response carries indexPoints and contributors, so it is only worth calling
// when those extras are wanted.
func (c *Client) GetEpisode(ctx context.Context, podcastID, episodeID string) (*Episode, error) {
	u := fmt.Sprintf("%s/radio/catalog/podcast/%s/episodes/%s",
		c.base(), url.PathEscape(podcastID), url.PathEscape(episodeID))
	var out Episode
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Manifest returns the playback manifest for an episode.
func (c *Client) Manifest(ctx context.Context, episodeID string) (*ManifestResponse, error) {
	u := c.base() + "/playback/manifest/podcast/" + url.PathEscape(episodeID)
	var out ManifestResponse
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AudioAsset returns the first usable audio asset for an episode, or
// ErrNotPlayable if NRK reports the episode as unplayable or lists no assets.
func (c *Client) AudioAsset(ctx context.Context, episodeID string) (*Asset, error) {
	m, err := c.Manifest(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	if m.Playability != "playable" {
		return nil, fmt.Errorf("%w: %s (playability=%q)", ErrNotPlayable, episodeID, m.Playability)
	}
	if m.Playable == nil || len(m.Playable.Assets) == 0 {
		return nil, fmt.Errorf("%w: %s (no assets)", ErrNotPlayable, episodeID)
	}
	for _, a := range m.Playable.Assets {
		if a.URL != "" && !a.Encrypted {
			return &a, nil
		}
	}
	return nil, fmt.Errorf("%w: %s (no unencrypted asset with a URL)", ErrNotPlayable, episodeID)
}

// AssetInfo is the size and content type of an audio file, for the RSS
// enclosure element.
type AssetInfo struct {
	Length int64
	Type   string
}

// HeadAsset issues a HEAD request against an audio URL, following redirects,
// and reports the file's size and content type. A missing or unparseable
// Content-Length yields a zero length rather than an error: some CDN nodes
// omit it, and an enclosure with length 0 is better than dropping the episode.
func (c *Client) HeadAsset(ctx context.Context, assetURL string) (AssetInfo, error) {
	resp, err := c.do(ctx, http.MethodHead, assetURL)
	if err != nil {
		return AssetInfo{}, err
	}
	defer resp.Body.Close()

	info := AssetInfo{Type: resp.Header.Get("Content-Type")}
	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			info.Length = n
		}
	}
	return info, nil
}
