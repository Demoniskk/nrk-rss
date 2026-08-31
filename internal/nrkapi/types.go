package nrkapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Image is a single rendition of an NRK image. The search endpoint
// (/radio/search/...) serialises the location as "uri" while the catalog
// endpoints use "url", so both are accepted and normalised by URL().
type Image struct {
	URL   string `json:"url"`
	URI   string `json:"uri"`
	Width int    `json:"width"`
}

// URL returns whichever of the two location fields the endpoint populated.
func (i Image) Location() string {
	if i.URL != "" {
		return i.URL
	}
	return i.URI
}

// Images is a set of renditions of the same image at different widths.
type Images []Image

// Widest returns the location of the highest-resolution rendition, or "" if
// the set is empty or every entry lacks a usable location.
func (imgs Images) Widest() string {
	best := ""
	bestWidth := -1
	for _, img := range imgs {
		loc := img.Location()
		if loc == "" {
			continue
		}
		if img.Width > bestWidth {
			best, bestWidth = loc, img.Width
		}
	}
	return best
}

// Titles is the title/subtitle pair NRK attaches to series and episodes.
type Titles struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
}

// Category is NRK's own genre classification for a series.
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SearchResponse is the payload of the podcast catalogue search endpoint.
type SearchResponse struct {
	Series []SearchSeries `json:"series"`
}

// SearchSeries is one entry in the catalogue listing. Type is "podcast" for
// real podcasts; "customSeason", "series" and "singleProgram" also appear and
// are not podcasts in their own right.
type SearchSeries struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	InitialCharacter string `json:"initialCharacter"`
	SeriesID         string `json:"seriesId"`
	SeasonID         string `json:"seasonId"`
	Images           Images `json:"images"`
	SquareImages     Images `json:"squareImages"`
}

// PodcastResponse is the payload of the single-podcast catalog endpoint.
type PodcastResponse struct {
	Type       string `json:"type"`
	SeriesType string `json:"seriesType"`
	Series     struct {
		ID          string   `json:"id"`
		Titles      Titles   `json:"titles"`
		Category    Category `json:"category"`
		Image       Images   `json:"image"`
		SquareImage Images   `json:"squareImage"`
	} `json:"series"`
}

// EpisodesResponse is one page of a podcast's episode list.
type EpisodesResponse struct {
	Embedded struct {
		Episodes []Episode `json:"episodes"`
	} `json:"_embedded"`
	Links struct {
		Next *struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
}

// HasNext reports whether another page of episodes follows this one.
func (r EpisodesResponse) HasNext() bool {
	return r.Links.Next != nil && r.Links.Next.Href != ""
}

// Episode is a single podcast episode as returned by both the episode-list
// endpoint and the single-episode endpoint. IndexPoints and Contributors are
// only populated by the latter.
type Episode struct {
	ID             string        `json:"id"`
	EpisodeID      string        `json:"episodeId"`
	Titles         Titles        `json:"titles"`
	Duration       string        `json:"duration"`
	Date           string        `json:"date"`
	Image          Images        `json:"image"`
	SquareImage    Images        `json:"squareImage"`
	ProductionYear int           `json:"productionYear"`
	IndexPoints    []IndexPoint  `json:"indexPoints"`
	Contributors   []Contributor `json:"contributors"`
	Links          struct {
		Season *struct {
			// The list endpoint returns the season slug in "name" and the
			// display name in "title"; older/other shapes use "id"/"name".
			ID    string `json:"id"`
			Name  string `json:"name"`
			Title string `json:"title"`
		} `json:"season"`
	} `json:"_links"`
}

// Season returns the season's stable identifier and its display name, or two
// empty strings if the episode is not part of a season.
func (e Episode) Season() (id, name string) {
	s := e.Links.Season
	if s == nil {
		return "", ""
	}
	id = s.ID
	if id == "" {
		id = s.Name
	}
	name = s.Title
	if name == "" {
		name = s.Name
	}
	return id, name
}

// PublishedAt parses the episode's publication timestamp.
func (e Episode) PublishedAt() (time.Time, error) {
	if e.Date == "" {
		return time.Time{}, fmt.Errorf("episode %s has no date", e.EpisodeID)
	}
	return time.Parse(time.RFC3339, e.Date)
}

// DurationSeconds returns the episode length in whole seconds, or 0 if NRK
// gave no parseable duration.
func (e Episode) DurationSeconds() int {
	d, err := ParseISODuration(e.Duration)
	if err != nil {
		return 0
	}
	return int(d.Seconds())
}

// IndexPoint is a chapter marker within an episode.
type IndexPoint struct {
	Title      string `json:"title"`
	StartPoint string `json:"startPoint"`
	PartID     int    `json:"partId"`
}

// StartSeconds returns the marker's offset from the start of the episode.
func (p IndexPoint) StartSeconds() int {
	d, err := ParseISODuration(p.StartPoint)
	if err != nil {
		return 0
	}
	return int(d.Seconds())
}

// Contributor is a person credited on an episode. NRK models Name as a list
// because a single credit line can cover several people.
type Contributor struct {
	Role string   `json:"role"`
	Name []string `json:"name"`
}

// ManifestResponse is the payload of the playback-manifest endpoint.
type ManifestResponse struct {
	Playability string `json:"playability"`
	Playable    *struct {
		Assets []Asset `json:"assets"`
	} `json:"playable"`
}

// Asset is one playable rendition of an episode's audio.
type Asset struct {
	URL       string `json:"url"`
	Format    string `json:"format"`
	MimeType  string `json:"mimeType"`
	Encrypted bool   `json:"encrypted"`
}

// ParseISODuration parses the restricted subset of ISO-8601 durations NRK
// emits for episode lengths and chapter offsets, e.g. "PT34M12S" or
// "PT1H2M3.5S". Only hours, minutes and seconds are supported; the date
// portion (years/months/days) does not occur in this API and is rejected.
func ParseISODuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	rest, ok := strings.CutPrefix(s, "PT")
	if !ok {
		return 0, fmt.Errorf("unsupported duration %q: missing PT prefix", s)
	}
	var total time.Duration
	num := strings.Builder{}
	seen := false
	for _, r := range rest {
		if (r >= '0' && r <= '9') || r == '.' || r == ',' {
			if r == ',' {
				r = '.' // NRK occasionally emits a decimal comma
			}
			num.WriteRune(r)
			continue
		}
		v, err := strconv.ParseFloat(num.String(), 64)
		if err != nil {
			return 0, fmt.Errorf("unsupported duration %q: bad number before %q", s, string(r))
		}
		num.Reset()
		switch r {
		case 'H':
			total += time.Duration(v * float64(time.Hour))
		case 'M':
			total += time.Duration(v * float64(time.Minute))
		case 'S':
			total += time.Duration(v * float64(time.Second))
		default:
			return 0, fmt.Errorf("unsupported duration %q: unknown unit %q", s, string(r))
		}
		seen = true
	}
	if !seen {
		return 0, fmt.Errorf("unsupported duration %q: no units", s)
	}
	return total, nil
}
