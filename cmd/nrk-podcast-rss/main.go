// Command nrk-podcast-rss generates standard podcast RSS feeds for every
// podcast on NRK Radio and writes them, with an index page, into a directory
// suitable for publishing on GitHub Pages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Demoniskk/nrk-rss/internal/nrkapi"
	"github.com/Demoniskk/nrk-rss/internal/scrape"
	"github.com/Demoniskk/nrk-rss/internal/site"
	"github.com/Demoniskk/nrk-rss/internal/store"
)

const generator = "nrk-podcast-rss"

const usage = `nrk-podcast-rss — RSS feeds for every podcast on NRK Radio.

Usage:
  nrk-podcast-rss <command> [flags]

Commands:
  scrape-all   Fetch the complete back catalogue of every podcast. Slow; run
               once, or again deliberately after a bug fix.
  update       Fetch only episodes not already in the state store. Cheap;
               meant to run daily.
  export       Regenerate the site from the state store without any NRK calls.

Run "nrk-podcast-rss <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	// Ctrl-C and the CI job timeout both cancel the run; work already
	// committed to the store survives, so the next run resumes from it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd := os.Args[1]; cmd {
	case "scrape-all":
		err = runScrape(ctx, os.Args[2:], scrape.ModeFull)
	case "update":
		err = runScrape(ctx, os.Args[2:], scrape.ModeIncremental)
	case "export":
		err = runExport(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Warn("run cancelled; state written so far is preserved")
			os.Exit(130)
		}
		slog.Error("run failed", "error", err)
		os.Exit(1)
	}
}

// commonFlags are the flags shared by every subcommand.
type commonFlags struct {
	db      string
	out     string
	baseURL string
	verbose bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.db, "db", "state/episodes.db", "path to the SQLite state database")
	fs.StringVar(&c.out, "out", "docs", "directory to write the generated site into")
	fs.StringVar(&c.baseURL, "base-url", "",
		"public root the site is served from, e.g. https://user.github.io/nrk-podcast-rss "+
			"(used for absolute feed URLs; optional)")
	fs.BoolVar(&c.verbose, "v", false, "log at debug level")
}

func (c *commonFlags) logger() *slog.Logger {
	level := slog.LevelInfo
	if c.verbose {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func runScrape(ctx context.Context, args []string, mode scrape.Mode) error {
	name := "update"
	if mode == scrape.ModeFull {
		name = "scrape-all"
	}

	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var common commonFlags
	common.register(fs)

	var (
		concurrency        = fs.Int("concurrency", 5, "podcasts to work on at once")
		episodeConcurrency = fs.Int("episode-concurrency", 3,
			"episodes to fetch at once within one podcast "+
				"(total in-flight requests stay under concurrency x this)")
		pageSize = fs.Int("page-size", 50, "episode-list page size requested from NRK")
		reqRate  = fs.Float64("rate", nrkapi.DefaultRate,
			"sustained NRK API requests per second across all workers; "+
				"NRK rate-limits hard, so raise this with care")
		userAgent = fs.String("user-agent", nrkapi.DefaultUserAgent, "User-Agent sent to NRK")
		rich      = fs.Bool("rich", false,
			"fetch chapter markers and contributor credits, at one extra API call per new episode")
		only = fs.String("only", "",
			"comma-separated podcast IDs to restrict the run to (default: the whole catalogue)")
		maxDuration = fs.Duration("max-duration", 0,
			"stop scraping cleanly after this long and still write the site "+
				"(e.g. 5h). Set it below a CI job timeout so a long backfill "+
				"commits its progress instead of being killed. 0 means no limit")
		skipExport = fs.Bool("skip-export", false, "do not regenerate the site afterwards")
		maxPages   *int
		force      *bool
	)

	if mode == scrape.ModeIncremental {
		maxPages = fs.Int("max-pages-per-podcast", 3,
			"safety cap on episode-list pages read per podcast before giving up on "+
				"finding a known episode; does not apply to podcasts with nothing stored yet")
	} else {
		force = fs.Bool("force", false,
			"re-fetch every episode even if it is already stored")
	}

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: nrk-podcast-rss %s [flags]\n\nFlags:\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := common.logger()

	st, err := store.Open(common.db)
	if err != nil {
		return err
	}
	defer st.Close()

	client := nrkapi.New(log)
	client.UserAgent = *userAgent
	client.SetRate(*reqRate, nrkapi.DefaultBurst)

	opts := scrape.Options{
		Mode:               mode,
		Concurrency:        *concurrency,
		EpisodeConcurrency: *episodeConcurrency,
		PageSize:           *pageSize,
		Rich:               *rich,
		Only:               splitList(*only),
	}
	if maxPages != nil {
		opts.MaxPagesPerPodcast = *maxPages
	}
	if force != nil {
		opts.Force = *force
	}

	// The scrape runs under its own deadline so a long backfill stops of its
	// own accord and still gets to write its output. The export below uses the
	// outer context, which that deadline does not touch.
	runCtx := ctx
	if *maxDuration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, *maxDuration)
		defer cancel()
		log.Info("scrape has a time budget", "max_duration", *maxDuration)
	}

	started := time.Now()
	res, runErr := scrape.New(client, st, log).Run(runCtx, opts)

	// Running out of the budget is a planned, clean stop, not a failure:
	// everything fetched so far is already in the database, and re-running
	// picks up where this left off.
	budgetHit := errors.Is(runErr, context.DeadlineExceeded)
	if budgetHit {
		log.Warn("time budget reached; stopping cleanly with partial progress",
			"elapsed", time.Since(started).Round(time.Second),
			"hint", "re-run to continue the backfill")
		runErr = nil
	}

	// A cancelled or partly failed run still has useful state, so the site is
	// regenerated from whatever made it into the store before reporting.
	if res != nil {
		log.Info("scrape finished",
			"command", name,
			"elapsed", time.Since(started).Round(time.Second),
			"podcasts_seen", res.PodcastsSeen,
			"succeeded", res.PodcastsSucceeded,
			"failed", res.PodcastsFailed,
			"episodes_added", res.EpisodesAdded,
			"episodes_skipped", res.EpisodesSkipped)
	}

	if !*skipExport && res != nil {
		failures := map[string]string{}
		if res.Failures != nil {
			failures = res.Failures
		}
		if _, err := site.Export(ctx, st, site.Options{
			OutDir:    common.out,
			BaseURL:   common.baseURL,
			Generator: generator,
			Failures:  failures,
			Logger:    log,
		}); err != nil {
			// An export failure matters even if the scrape went fine.
			if runErr == nil {
				return err
			}
			log.Error("export failed", "error", err)
		}
	}

	if runErr != nil {
		return runErr
	}

	// Individual podcasts are allowed to fail; a run where nothing at all
	// succeeded is a real failure worth a non-zero exit. A run cut short by its
	// own time budget is exempt: it stopped on purpose, not because the
	// podcasts were broken.
	if !budgetHit && res.PodcastsSeen > 0 && res.PodcastsSucceeded == 0 {
		return fmt.Errorf("all %d podcasts failed", res.PodcastsSeen)
	}
	if res.PodcastsFailed > 0 {
		log.Warn("some podcasts failed", "failed", res.PodcastsFailed, "succeeded", res.PodcastsSucceeded)
	}
	return nil
}

func runExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: nrk-podcast-rss export [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := common.logger()

	st, err := store.Open(common.db)
	if err != nil {
		return err
	}
	defer st.Close()

	_, err = site.Export(ctx, st, site.Options{
		OutDir:    common.out,
		BaseURL:   common.baseURL,
		Generator: generator,
		Logger:    log,
	})
	return err
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
