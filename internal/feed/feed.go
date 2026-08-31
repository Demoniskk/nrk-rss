// Package feed renders stored podcast state as RSS 2.0 with the iTunes
// podcast namespace, plus a few Podcasting 2.0 extras where the data allows.
package feed

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Demoniskk/nrk-rss/internal/store"
)

// Namespace URIs declared on every generated feed.
const (
	nsITunes  = "http://www.itunes.com/dtds/podcast-1.0.dtd"
	nsPodcast = "https://podcastindex.org/namespace/1.0"
	nsPSC     = "http://podlove.org/simple-chapters"
	nsAtom    = "http://www.w3.org/2005/Atom"
)

// PodcastPageURL is where a podcast lives on NRK Radio's own site.
func PodcastPageURL(podcastID string) string {
	return "https://radio.nrk.no/podkast/" + podcastID
}

// EpisodePageURL is the NRK Radio page for a single episode.
func EpisodePageURL(podcastID, episodeID string) string {
	return "https://radio.nrk.no/podkast/" + podcastID + "/" + episodeID
}

// RSS is the document root.
//
// Namespaces are declared as plain attributes and element names carry literal
// prefixes, because encoding/xml cannot emit prefixed names on its own.
type RSS struct {
	XMLName      xml.Name `xml:"rss"`
	Version      string   `xml:"version,attr"`
	XMLNSITunes  string   `xml:"xmlns:itunes,attr"`
	XMLNSPodcast string   `xml:"xmlns:podcast,attr"`
	XMLNSPSC     string   `xml:"xmlns:psc,attr"`
	XMLNSAtom    string   `xml:"xmlns:atom,attr"`
	Channel      Channel  `xml:"channel"`
}

// Channel is the feed-level metadata.
type Channel struct {
	Title          string          `xml:"title"`
	Link           string          `xml:"link"`
	Description    string          `xml:"description"`
	Language       string          `xml:"language"`
	Copyright      string          `xml:"copyright"`
	LastBuildDate  string          `xml:"lastBuildDate,omitempty"`
	Generator      string          `xml:"generator,omitempty"`
	AtomLink       *AtomLink       `xml:"atom:link,omitempty"`
	ITunesAuthor   string          `xml:"itunes:author"`
	ITunesSummary  string          `xml:"itunes:summary,omitempty"`
	ITunesOwner    *ITunesOwner    `xml:"itunes:owner,omitempty"`
	ITunesImage    *ITunesImage    `xml:"itunes:image,omitempty"`
	ITunesCategory *ITunesCategory `xml:"itunes:category,omitempty"`
	ITunesExplicit string          `xml:"itunes:explicit,omitempty"`
	ITunesType     string          `xml:"itunes:type,omitempty"`
	Image          *Image          `xml:"image,omitempty"`
	Items          []Item          `xml:"item"`
}

// AtomLink is the self-referential feed link recommended by the RSS best
// practices profile.
type AtomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

// ITunesOwner is the feed's contact block.
type ITunesOwner struct {
	Name  string `xml:"itunes:name,omitempty"`
	Email string `xml:"itunes:email,omitempty"`
}

// ITunesImage is the artwork reference used at channel and item level.
type ITunesImage struct {
	Href string `xml:"href,attr"`
}

// ITunesCategory is the feed's iTunes genre.
type ITunesCategory struct {
	Text string `xml:"text,attr"`
}

// Image is the plain RSS 2.0 channel image, for readers that ignore iTunes tags.
type Image struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

// Item is one episode.
type Item struct {
	Title          string       `xml:"title"`
	Link           string       `xml:"link,omitempty"`
	Description    string       `xml:"description"`
	GUID           GUID         `xml:"guid"`
	PubDate        string       `xml:"pubDate"`
	Enclosure      Enclosure    `xml:"enclosure"`
	ITunesTitle    string       `xml:"itunes:title,omitempty"`
	ITunesSummary  string       `xml:"itunes:summary,omitempty"`
	ITunesDuration string       `xml:"itunes:duration,omitempty"`
	ITunesImage    *ITunesImage `xml:"itunes:image,omitempty"`
	ITunesExplicit string       `xml:"itunes:explicit,omitempty"`
	PodcastSeason  *Season      `xml:"podcast:season,omitempty"`
	PodcastPersons []Person     `xml:"podcast:person,omitempty"`
	Chapters       *PSCChapters `xml:"psc:chapters,omitempty"`
}

// GUID is the item's globally unique identifier. NRK episode IDs are stable
// and not resolvable URLs, so isPermaLink is always false.
type GUID struct {
	IsPermaLink bool   `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

// Enclosure points at the audio file NRK already serves.
type Enclosure struct {
	URL    string `xml:"url,attr"`
	Type   string `xml:"type,attr"`
	Length int64  `xml:"length,attr"`
}

// Season is the Podcasting 2.0 season marker. Its value must be a number, so
// it is only emitted for podcasts whose season IDs are numeric.
type Season struct {
	Name   string `xml:"name,attr,omitempty"`
	Number int    `xml:",chardata"`
}

// Person is a Podcasting 2.0 credit.
type Person struct {
	Role  string `xml:"role,attr,omitempty"`
	Value string `xml:",chardata"`
}

// PSCChapters carries chapter markers inline, in the Podlove Simple Chapters
// format. Inline markers avoid publishing a separate JSON file per episode.
type PSCChapters struct {
	Version  string       `xml:"version,attr"`
	Chapters []PSCChapter `xml:"psc:chapter"`
}

// PSCChapter is a single inline chapter marker.
type PSCChapter struct {
	Start string `xml:"start,attr"`
	Title string `xml:"title,attr"`
}

// Options tunes how a feed is rendered.
type Options struct {
	// SelfLink is the public URL this feed will be served from. Left empty,
	// the atom:self link is omitted.
	SelfLink string
	// Generator names the tool in the feed. Optional.
	Generator string
	// Language is the RSS language code. Defaults to "no".
	Language string
}

// Build renders a podcast and its episodes into an RSS document.
func Build(p store.Podcast, episodes []store.Episode, opts Options) *RSS {
	lang := opts.Language
	if lang == "" {
		lang = "no"
	}

	ch := Channel{
		Title:         p.Title,
		Link:          PodcastPageURL(p.ID),
		Description:   p.Description,
		Language:      lang,
		Copyright:     "NRK",
		Generator:     opts.Generator,
		ITunesAuthor:  "NRK",
		ITunesSummary: p.Description,
		ITunesOwner:   &ITunesOwner{Name: "NRK"},
		// NRK does not expose a per-podcast explicit flag; "no" is the
		// accurate default for a public broadcaster's published catalogue.
		ITunesExplicit: "no",
		ITunesType:     "episodic",
	}

	// lastBuildDate tracks when the feed's content last changed, not when this
	// tool last ran. Deriving it from the newest episode keeps a feed with no
	// new episodes byte-identical between runs, which is what lets the daily
	// job skip committing anything on a quiet day.
	if len(episodes) > 0 {
		ch.LastBuildDate = episodes[0].PubDate.UTC().Format(time.RFC1123Z)
	}
	if opts.SelfLink != "" {
		ch.AtomLink = &AtomLink{Href: opts.SelfLink, Rel: "self", Type: "application/rss+xml"}
	}
	if p.ImageURL != "" {
		ch.ITunesImage = &ITunesImage{Href: p.ImageURL}
		ch.Image = &Image{URL: p.ImageURL, Title: p.Title, Link: PodcastPageURL(p.ID)}
	}
	if cat := itunesCategory(p.CategoryName, p.CategoryID); cat != "" {
		ch.ITunesCategory = &ITunesCategory{Text: cat}
	}

	ch.Items = make([]Item, 0, len(episodes))
	for _, e := range episodes {
		ch.Items = append(ch.Items, buildItem(p, e))
	}

	return &RSS{
		Version:      "2.0",
		XMLNSITunes:  nsITunes,
		XMLNSPodcast: nsPodcast,
		XMLNSPSC:     nsPSC,
		XMLNSAtom:    nsAtom,
		Channel:      ch,
	}
}

func buildItem(p store.Podcast, e store.Episode) Item {
	item := Item{
		Title:       e.Title,
		Link:        EpisodePageURL(p.ID, e.EpisodeID),
		Description: e.Description,
		GUID:        GUID{IsPermaLink: false, Value: e.EpisodeID},
		PubDate:     e.PubDate.Format(time.RFC1123Z),
		Enclosure: Enclosure{
			URL:    e.EnclosureURL,
			Type:   e.EnclosureType,
			Length: e.EnclosureLength,
		},
		ITunesTitle:    e.Title,
		ITunesSummary:  e.Description,
		ITunesExplicit: "no",
	}

	if e.DurationSeconds > 0 {
		item.ITunesDuration = strconv.Itoa(e.DurationSeconds)
	}
	if e.ImageURL != "" {
		item.ITunesImage = &ITunesImage{Href: e.ImageURL}
	}

	// podcast:season requires a numeric value; NRK season IDs are sometimes
	// slugs ("11-september"), in which case the tag is skipped rather than
	// emitted invalid.
	if n, err := strconv.Atoi(strings.TrimSpace(e.SeasonID)); err == nil {
		item.PodcastSeason = &Season{Number: n, Name: e.SeasonName}
	}

	for _, c := range e.Contributors {
		for _, name := range c.Name {
			if name = strings.TrimSpace(name); name != "" {
				item.PodcastPersons = append(item.PodcastPersons, Person{Role: c.Role, Value: name})
			}
		}
	}

	if len(e.IndexPoints) > 0 {
		chapters := make([]PSCChapter, 0, len(e.IndexPoints))
		for _, ip := range e.IndexPoints {
			chapters = append(chapters, PSCChapter{
				Start: formatChapterStart(ip.StartSeconds),
				Title: ip.Title,
			})
		}
		item.Chapters = &PSCChapters{Version: "1.2", Chapters: chapters}
	}

	return item
}

// formatChapterStart renders an offset as the hh:mm:ss form Podlove expects.
func formatChapterStart(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, (seconds%3600)/60, seconds%60)
}

// itunesCategory maps an NRK category onto the closest Apple Podcasts
// top-level category, so feeds validate against Apple's fixed list. Unmapped
// categories fall back to the NRK name, which readers will show verbatim.
func itunesCategory(name, id string) string {
	switch strings.ToLower(id) {
	case "humor":
		return "Comedy"
	case "nyheter", "samfunn":
		return "News"
	case "sport":
		return "Sports"
	case "kultur", "litteratur", "musikk":
		return "Arts"
	case "vitenskap", "natur":
		return "Science"
	case "historie", "dokumentar":
		return "History"
	case "barn", "familie":
		return "Kids & Family"
	case "livsstil", "helse":
		return "Health & Fitness"
	case "religion":
		return "Religion & Spirituality"
	case "lyddrama", "hoerespill":
		return "Fiction"
	case "sapmi":
		return "Society & Culture"
	}
	return name
}

// Render writes the feed as XML with a declaration and indentation.
func Render(w io.Writer, r *RSS) error {
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return fmt.Errorf("feed: writing XML header: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("feed: encoding feed: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("feed: flushing feed: %w", err)
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteFile renders the feed to path, writing via a temporary file so a failed
// run cannot leave a truncated feed behind. A feed whose bytes are unchanged
// is left alone, so an unchanged podcast produces no git diff at all.
func WriteFile(path string, r *RSS) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("feed: creating %s: %w", dir, err)
		}
	}

	var rendered strings.Builder
	if err := Render(&rendered, r); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == rendered.String() {
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".feed-*.xml")
	if err != nil {
		return fmt.Errorf("feed: creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := io.WriteString(tmp, rendered.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("feed: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("feed: closing temp file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("feed: writing %s: %w", path, err)
	}
	return nil
}

// SafeFilename reports whether a podcast ID is safe to use as a filename, so a
// hostile or malformed ID cannot escape the output directory.
func SafeFilename(podcastID string) bool {
	if podcastID == "" || podcastID == "." || podcastID == ".." {
		return false
	}
	return !strings.ContainsAny(podcastID, `/\:*?"<>|`) && !strings.Contains(podcastID, "..")
}
