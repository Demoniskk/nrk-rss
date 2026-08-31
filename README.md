# nrk-rss

Podcast RSS feeds for every podcast on NRK Radio.

NRK Radio doesn't publish RSS feeds you can paste into a normal podcast app, so
this makes them: one standard feed per podcast, regenerated daily.

**https://demoniskk.github.io/nrk-rss/**

Search that page for a podcast, copy its feed link, and paste it into your
podcast app. No account, nothing to install. The feeds work in Apple Podcasts,
Pocket Casts, Overcast, AntennaPod, or anything else that takes an RSS URL.

Each feed carries the podcast's artwork and description, plus every episode with
its own title, description, publication date and audio. The audio links point
straight at the files NRK already publishes — nothing is re-hosted.

## What it does

A small Go program that, once a day:

1. Asks NRK's public API which podcasts exist (235 of them at the moment).
2. Checks each one for episodes it hasn't seen before.
3. Fetches only those new episodes.
4. Rewrites the RSS feeds and the website.

The "only new episodes" part is the whole point. There are tens of thousands of
episodes in the catalogue, and published episodes never change, so re-fetching
them daily would be pointless. A record of everything already fetched is kept in
a small database, and a normal day's update costs about three requests per
podcast and finishes in seconds.

## Setup (if you're forking this)

1. **Settings → Pages → Source: GitHub Actions**
2. **Actions → `scrape-all` → Run workflow.** This is the one-time backfill of
   the entire back catalogue. It takes hours and will probably need running more
   than once — it stops itself before the job timeout and resumes where it left
   off, so just re-run it until it reports no new episodes.
3. Done. The `update` workflow then runs daily at 03:17 UTC and keeps everything
   current. You can also trigger it by hand from the Actions tab.

### Running it locally

```bash
go build -o nrk-rss ./cmd/nrk-rss

./nrk-rss scrape-all --only desken_brenner   # try one podcast
./nrk-rss scrape-all                         # the whole catalogue
./nrk-rss update                             # only what's new
./nrk-rss export                             # rebuild the site
```

Then open `docs/index.html`. Run any command with `-h` to see its flags.

## Under the hood

Written in Go, no dependencies beyond a pure-Go SQLite driver and a rate
limiter. The generated site is plain HTML, CSS and a little vanilla JavaScript —
no build step, no framework.

```
cmd/nrk-rss/   CLI
internal/nrkapi/       NRK API client, rate limiting, retries
internal/store/        SQLite state store
internal/scrape/       works out what's new, fetches it
internal/feed/         RSS/iTunes XML generation
internal/site/         index.html and feeds.json
```

**[TECHNICAL.md](TECHNICAL.md)** — how the incremental update works, what NRK's
API does that its shape doesn't suggest, how its rate limiter actually behaves,
and the bugs found along the way.

## Scope

This publishes NRK's own publicly listed episode metadata and links to audio
files NRK already serves publicly. No audio is downloaded, re-hosted or
modified.

Licensed under GPL-3.0 (see [LICENSE](LICENSE)).
