# Technical notes

Design notes and findings from building [nrk-rss](README.md). Most of
the interesting parts were things NRK's API does that its shape doesn't suggest.

- [Architecture](#architecture)
- [The update/scrape split](#the-updatescrape-split)
- [Why the database is committed](#why-the-database-is-committed)
- [Findings](#findings)
  - [NRK rate-limits hard, and punishes you for arguing](#nrk-rate-limits-hard-and-punishes-you-for-arguing)
  - [A CI timeout would have silently destroyed the backfill](#a-ci-timeout-would-have-silently-destroyed-the-backfill)
  - [Two Pages deployments raced each other](#two-pages-deployments-raced-each-other)
  - [Making "no empty commits" actually true](#making-no-empty-commits-actually-true)
  - [What "every podcast" actually means](#what-every-podcast-actually-means)
  - [The page size caps at exactly 50](#the-page-size-caps-at-exactly-50)
  - [Two API details that differ from the obvious reading](#two-api-details-that-differ-from-the-obvious-reading)
  - [A page cap that would have left permanent holes](#a-page-cap-that-would-have-left-permanent-holes)
  - [Smaller ones](#smaller-ones)
- [The NRK API](#the-nrk-api)
- [Testing](#testing)

---

## Architecture

```
NRK JSON API  ──▶  internal/nrkapi   client, rate limiting, retries
                        │
                        ▼
                   internal/scrape   diff against stored IDs, fetch the rest
                        │
                        ▼
                   internal/store    SQLite: state/episodes.db
                        │
                        ▼
          internal/feed + internal/site
                        │
                        ▼
   docs/feeds/*.xml · docs/index.html · docs/feeds.json  ──▶  GitHub Pages
```

One binary, three commands:

| Command | What it does | When |
| --- | --- | --- |
| `scrape-all` | Pages through **every episode** of every podcast. | Once, to seed the database. |
| `update` | Fetches **only episodes not already stored**. | Daily, from Actions. |
| `export` | Rebuilds the site from the database. No NRK calls. | After template changes. |

Two workflows drive it: `scrape-all.yml` is manual only, `update.yml` runs daily
at 03:17 UTC plus a manual trigger. `update.yml` runs the tests, runs `update`,
commits `state/episodes.db` and `docs/` **only if something changed**, then
deploys to Pages via `upload-pages-artifact` + `deploy-pages` (rather than
pointing Pages at the `docs/` folder, so the deploy is visible in the Actions
tab).

Feeds are RSS 2.0 + the iTunes namespace, plus `<podcast:season>` and — behind
`--rich` — `<podcast:person>` and inline Podlove chapter markers. XML is
generated with `encoding/xml`, so `&`, `<` and quotes in titles escape properly
instead of producing invalid feeds.

## The update/scrape split

Building one episode's `<item>` costs three API calls:

1. The episode metadata — free, it arrives with the episode list.
2. A playback-manifest lookup, for the audio URL.
3. A `HEAD` on that URL, for the enclosure's byte length and content type.

Published episodes never change, so re-fetching them daily is pure waste — one
podcast in this catalogue has over 800 episodes. So `state/episodes.db` stores
everything needed to re-render an episode's `<item>` offline, and `update` asks
NRK for the newest page of episodes, stops at the first episode ID it already
has, and fetches only what came before it.

A quiet day is about three requests per podcast. A full backfill is tens of
thousands.

## Why the database is committed

`state/episodes.db` is checked into the repo deliberately, rather than restored
from an Actions cache. A lost cache would silently re-do work; a lost database
with nothing to fall back on would silently **skip** episodes, because `update`
would have nothing to diff against and would treat the newest page as the whole
podcast. Correctness depends on that state surviving, so it is committed. The
WAL sidecar files are gitignored.

A `.gitattributes` marks `*.db` as binary. Without it, `core.autocrlf` on
Windows would happily "fix" line endings inside the SQLite file and corrupt it.

---

## Findings

### NRK rate-limits hard, and punishes you for arguing

The first realistic run failed **11 of 14 podcasts** with HTTP 429. Measuring
against the live API:

- A 429 comes back with `Retry-After: 600` — ten minutes.
- The penalty **persists while requests keep arriving**. Probing at a mere
  1 req/s *during* a penalty window still returned 429 about half the time.
  After 600 seconds of complete silence, 15 consecutive requests at 1 req/s all
  succeeded.
- Once clear, 3 req/s over 90 requests and 5 req/s over 50 requests were both
  clean.

So it isn't a simple token bucket you can nibble at. Continuing to send is what
keeps you locked out.

The fix that actually mattered: **a 429 pauses every worker, not just the one
that received it** (`internal/nrkapi/throttle.go`). Backing off a single
goroutine while nineteen others keep hammering is precisely what keeps the
penalty alive — which is exactly what the failing run was doing.

Alongside that, all requests share one rate limiter (4 req/s for the API; audio
`HEAD`s get their own budget, since they hit a different host), so worker
concurrency controls parallelism rather than request rate. Requests already in
flight can't be recalled, so the guarantee is specifically about requests that
*start* after the 429 — which is what the test asserts.

The same 14 podcasts then completed with zero failures.

### A CI timeout would have silently destroyed the backfill

At a polite request rate, `scrape-all` takes longer than GitHub's six-hour job
limit. The trap: when GitHub kills a job at its timeout, **subsequent steps
never run** — so the commit step never fires and the entire backfill is lost
with nothing to show for it.

Hence `--max-duration`. The scraper stops itself at 300 minutes, exits zero, and
the commit step still runs. Because a run skips episodes already stored,
re-running the workflow continues from where the last one stopped.

Verified: a budget-truncated run exits 0 with partial state committed, and
re-running resumed 11 → 52 episodes without re-fetching anything.

One subtlety this exposed: the rate limiter refuses a request as soon as it can
see the wait would outlast the context deadline, which is *before* the deadline
actually arrives. So `ctx.Err()` is still nil, and the first version reported 12
podcasts as having **failed** when the run had simply ended. The limiter now
reports that refusal as the context error, and the scraper treats it as the run
ending rather than as broken podcasts.

### Two Pages deployments raced each other

The `update` workflow originally finished with `actions/upload-pages-artifact`
and an `actions/deploy-pages` job — the modern GitHub Actions way to publish.
But Pages was also configured the classic way, deploying from `main`'s `docs/`
directory, which makes GitHub run its own `pages-build-deployment` on every
push.

Each run therefore created two deployments to the same `github-pages`
environment, seconds apart: one from the deploy job, one from the commit that
job had just pushed. Usually both got through. Once they overlapped closely
enough that the later one lost:

```
09:50:49  deployment  sha 4d92450  (deploy-pages job)      -> success  09:51:05
09:50:54  deployment  sha 3027129  (push-triggered build)  -> failure  09:51:02
```

Nothing was actually broken — the loser of the race is redundant by definition,
and the site served the right content throughout. But it surfaced as a red run
in the Actions tab, which is the kind of noise that teaches you to stop reading
red runs.

The fix was to delete the deploy job and keep the branch-based deployment.
One publisher instead of two, no `pages: write` or `id-token: write`
permissions, and no tarring and uploading 80 MB of `docs/` on every run. It
also fixed something separate: only `update` had the deploy job, so a fresh
`scrape-all` used to leave the site untouched until the next daily run. Now any
push publishes.

### Making "no empty commits" actually true

The workflow is supposed to skip committing when nothing changed. Naively it
never would have, because three things churned on every single run:

- `lastBuildDate` in each feed.
- `generated_at` in `feeds.json`.
- `last_scraped` in the podcasts table, which rewrote the committed database.

So: `lastBuildDate` is derived from the newest episode rather than the run time;
`feeds.json` is left untouched when `generated_at` is the *only* difference; the
index page shows the newest episode date rather than a build timestamp; and the
podcast upsert is conditional on something having actually changed:

```sql
ON CONFLICT(podcast_id) DO UPDATE SET ...
WHERE podcasts.title IS NOT excluded.title
   OR podcasts.description IS NOT excluded.description
   OR ...
```

A no-op run now leaves every file **and the database itself** byte-identical.
Verified by checksumming the whole output tree and the `.db` before and after.

### What "every podcast" actually means

The catalogue call returns 597 entries: **235 of type `podcast` and 362 of type
`customSeason`**. Only the 235 are scraped, which sounds like dropping more than
half the catalogue. It isn't:

- The 362 custom seasons belong to just **26 distinct parent series, and all 26
  are already among the 235**. They're alternate views of existing podcasts, not
  separate ones.
- Checked directly: all 7 episodes of the `11-september` custom season also
  appear in the episode list of its parent podcast `krig_og_fred`. Scraping the
  parent picks them up.

The `letters` index in the same response sums to exactly 597, matching the
entries returned, which confirms `take=1000` isn't truncating the catalogue and
no pagination is needed.

Two things are still legitimately skipped: episodes NRK reports as non-playable
(geo-blocked or expired), which are logged and counted rather than failing their
podcast; and podcasts that end up with no playable episodes at all, which get no
feed file, because an empty feed isn't subscribable.

### The page size caps at exactly 50

```
pageSize=50  -> HTTP 200
pageSize=51  -> HTTP 400
pageSize=100 -> HTTP 400
```

A 400 is not retryable, so an over-large `--page-size` would have failed *every*
podcast in the run rather than degrading. The default was already 50, so this
was latent, but the flag accepted anything. The client now clamps to
`MaxPageSize = 50`.

### Two API details that differ from the obvious reading

- The search endpoint (`/radio/search/categories/podcast`) serialises image
  locations as **`uri`**. Every catalog endpoint uses **`url`**. Both are
  handled, and `Images.Widest()` normalises them.
- `indexPoints` (chapters) and `contributors` (credits) appear **only** on the
  single-episode endpoint — never in the episode list, despite the list
  otherwise returning the same shape. Chapters and credits therefore genuinely
  cost a fourth call per episode, which is why they sit behind `--rich` rather
  than being on by default.

### A page cap that would have left permanent holes

`update` stops at the first episode it already knows, with
`--max-pages-per-podcast` (default 3) as a safety cap for a podcast that deleted
and re-added its whole catalogue.

Applying that cap to a podcast with *nothing* stored yet would be a quiet bug:
it would fetch only the newest few pages, and every later run would stop at the
newest episode it already had and never look further back — a permanent hole no
amount of re-running would fill.

So the cap deliberately doesn't apply to podcasts that are new to the database.
They get backfilled in full, which means new podcasts appearing on NRK are
picked up completely by the daily job without another `scrape-all`.

### Smaller ones

- NRK's manifests report `audio/mp3`, which isn't a registered MIME type. The
  `HEAD` response gives the correct `audio/mpeg`, so the served type wins.
- Episode audio URLs redirect to a CDN. The feeds keep the stable
  `podkast.nrk.no` URL rather than the redirect target, which may expire.
- `podcast:season` requires a numeric value, but NRK season IDs are sometimes
  slugs like `11-september`. The tag is skipped rather than emitted invalid.
- NRK's own categories (`humor`, `lyddrama`, `sapmi`, …) aren't Apple Podcasts
  categories. They're mapped where there's a sensible equivalent, and fall back
  to NRK's own name otherwise.
- SQLite's `NOCASE` collation only folds ASCII, so "Podkast To" sorts before
  "Podkast Én". This broke a test that assumed otherwise — worth knowing before
  relying on `ORDER BY title COLLATE NOCASE` for non-English titles.
- Feeds and site files are written to a temp file and renamed, so an interrupted
  run can't leave a truncated feed behind.

---

## The NRK API

All endpoints are unauthenticated `GET`s against `https://psapi.nrk.no`, sent
with `Accept: application/json;api-version=3.5`.

| Purpose | Endpoint |
| --- | --- |
| List all podcasts | `/radio/search/categories/podcast?take=1000` |
| Podcast metadata | `/radio/catalog/podcast/{id}` |
| Episode list (paged) | `/radio/catalog/podcast/{id}/episodes?pageSize={n}&page={p}` |
| Single episode | `/radio/catalog/podcast/{id}/episodes/{episodeId}` |
| Playback manifest | `/playback/manifest/podcast/{episodeId}` |
| Audio size and type | `HEAD` on the manifest's asset URL |

Episode lists are newest-first and paginate via `_links.next`, which is absent
on the last page.

## Testing

`go test ./...` covers the API client, the state store, feed rendering, the site
export and the scrape logic. The parts worth knowing about:

- **Request-count assertions.** A fake NRK server counts requests per endpoint,
  so the tests pin down exactly how much work `update` is allowed to do: given
  two new episodes among 122 stored, it must make exactly one episode-list call,
  two manifest calls and two `HEAD`s — and zero episode-detail calls without
  `--rich`.
- **The rate-limit pause** is tested with a server that 429s once, asserting a
  request started afterwards is held back.
- **Feed rendering** is re-parsed as XML to prove special characters escaped
  rather than merely checking for substrings.
- **The no-op upsert** has its own test, because a regression there would
  silently reintroduce a daily empty commit.

Everything runs under `-race`.
