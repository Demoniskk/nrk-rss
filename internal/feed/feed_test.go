package feed

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/Demoniskk/nrk-rss/internal/store"
)

func sampleFeed() *RSS {
	p := store.Podcast{
		ID:           "desken_brenner",
		Title:        "Desken brenner",
		Description:  "Alt kan skje & ingen er trygge",
		CategoryID:   "humor",
		CategoryName: "Humor og prateprogram",
		ImageURL:     "https://gfx.nrk.no/square.jpg",
		LastScraped:  time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}
	eps := []store.Episode{{
		PodcastID:       "desken_brenner",
		EpisodeID:       "l_37996af7",
		Title:           `En "kort" beskjed <ja>`,
		Description:     "Vi er tilbake & det blir bra",
		PubDate:         time.Date(2026, 8, 28, 6, 0, 0, 0, time.FixedZone("CEST", 2*3600)),
		DurationSeconds: 2336,
		EnclosureURL:    "https://podkast.nrk.no/fil/a.mp3",
		EnclosureType:   "audio/mpeg",
		EnclosureLength: 289772,
		ImageURL:        "https://gfx.nrk.no/ep.jpg",
		SeasonID:        "2026",
		SeasonName:      "Sesong 2026",
		IndexPoints:     []store.IndexPoint{{Title: "Netthets & mer", StartSeconds: 218}},
		Contributors:    []store.Contributor{{Role: "Programleder", Name: []string{"Odd-Magnus Williamson"}}},
	}}
	return Build(p, eps, Options{
		SelfLink:  "https://example.github.io/feeds/desken_brenner.xml",
		Generator: "nrk-rss",
	})
}

func TestRenderIsWellFormedAndEscaped(t *testing.T) {
	var sb strings.Builder
	if err := Render(&sb, sampleFeed()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()

	// The document must parse back cleanly, which is the real check that
	// special characters in titles were escaped rather than concatenated in.
	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("re-parsing rendered feed: %v\n%s", err, out)
		}
	}

	for _, want := range []string{
		`<rss version="2.0"`,
		`xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"`,
		`xmlns:podcast="https://podcastindex.org/namespace/1.0"`,
		`<link>https://radio.nrk.no/podkast/desken_brenner</link>`,
		`<language>no</language>`,
		`<copyright>NRK</copyright>`,
		`<itunes:author>NRK</itunes:author>`,
		`<itunes:image href="https://gfx.nrk.no/square.jpg">`,
		`<itunes:category text="Comedy">`,
		`<guid isPermaLink="false">l_37996af7</guid>`,
		`<pubDate>Fri, 28 Aug 2026 06:00:00 +0200</pubDate>`,
		`<enclosure url="https://podkast.nrk.no/fil/a.mp3" type="audio/mpeg" length="289772">`,
		`<itunes:duration>2336</itunes:duration>`,
		`<podcast:season name="Sesong 2026">2026</podcast:season>`,
		`<podcast:person role="Programleder">Odd-Magnus Williamson</podcast:person>`,
		`<psc:chapter start="00:03:38" title="Netthets &amp; mer">`,
		`En &#34;kort&#34; beskjed &lt;ja&gt;`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered feed missing %q\n---\n%s", want, out)
		}
	}

	// A raw, unescaped ampersand would make the document invalid.
	if strings.Contains(out, "& ingen") {
		t.Error("channel description contains an unescaped ampersand")
	}
}

func TestNonNumericSeasonIsSkipped(t *testing.T) {
	p := store.Podcast{ID: "krig_og_fred", Title: "Krig og fred"}
	eps := []store.Episode{{
		EpisodeID:     "l_1",
		Title:         "Ep",
		PubDate:       time.Now(),
		EnclosureURL:  "https://x/a.mp3",
		EnclosureType: "audio/mpeg",
		SeasonID:      "11-september",
		SeasonName:    "11. september",
	}}
	var sb strings.Builder
	if err := Render(&sb, Build(p, eps, Options{})); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(sb.String(), "podcast:season") {
		t.Error("emitted podcast:season for a non-numeric season id")
	}
}

func TestSafeFilename(t *testing.T) {
	safe := []string{"desken_brenner", "abels-taarn", "p3morgen"}
	unsafe := []string{"", ".", "..", "../etc/passwd", `a\b`, "a/b", "a:b", "a?b", "a*b"}

	for _, s := range safe {
		if !SafeFilename(s) {
			t.Errorf("SafeFilename(%q) = false, want true", s)
		}
	}
	for _, s := range unsafe {
		if SafeFilename(s) {
			t.Errorf("SafeFilename(%q) = true, want false", s)
		}
	}
}
