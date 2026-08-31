package nrkapi

import (
	"testing"
	"time"
)

func TestParseISODuration(t *testing.T) {
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"PT34M12S", 34*time.Minute + 12*time.Second},
		{"PT12S", 12 * time.Second},
		{"PT1H", time.Hour},
		{"PT1H2M3S", time.Hour + 2*time.Minute + 3*time.Second},
		{"PT2M0.5S", 2*time.Minute + 500*time.Millisecond},
		{"PT2M0,5S", 2*time.Minute + 500*time.Millisecond},
		{"PT0S", 0},
	}
	for _, c := range ok {
		got, err := ParseISODuration(c.in)
		if err != nil {
			t.Errorf("ParseISODuration(%q) returned %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseISODuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	for _, in := range []string{"", "34M12S", "PT", "P1D", "PT1X", "PTxS"} {
		if _, err := ParseISODuration(in); err == nil {
			t.Errorf("ParseISODuration(%q) succeeded, want an error", in)
		}
	}
}

func TestImagesWidest(t *testing.T) {
	// The catalog endpoints use "url"; the search endpoint uses "uri".
	catalog := Images{{URL: "small", Width: 300}, {URL: "big", Width: 1920}, {URL: "mid", Width: 960}}
	if got := catalog.Widest(); got != "big" {
		t.Errorf("Widest() = %q, want big", got)
	}

	search := Images{{URI: "s", Width: 300}, {URI: "l", Width: 1600}}
	if got := search.Widest(); got != "l" {
		t.Errorf("Widest() over uri-form images = %q, want l", got)
	}

	if got := (Images{}).Widest(); got != "" {
		t.Errorf("Widest() on an empty set = %q, want empty", got)
	}
	if got := (Images{{Width: 100}}).Widest(); got != "" {
		t.Errorf("Widest() over locationless images = %q, want empty", got)
	}
}

func TestEpisodeSeason(t *testing.T) {
	// The episode-list endpoint puts the slug in "name" and the display name
	// in "title".
	var listShape Episode
	listShape.Links.Season = &struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Title string `json:"title"`
	}{Name: "2026", Title: "Sesong 2026"}

	id, name := listShape.Season()
	if id != "2026" || name != "Sesong 2026" {
		t.Errorf("Season() = %q, %q; want 2026, Sesong 2026", id, name)
	}

	var none Episode
	if id, name := none.Season(); id != "" || name != "" {
		t.Errorf("Season() with no season = %q, %q; want empty", id, name)
	}
}

func TestEpisodeDurationSecondsIsLenient(t *testing.T) {
	e := Episode{Duration: "PT34M12S"}
	if got := e.DurationSeconds(); got != 2052 {
		t.Errorf("DurationSeconds() = %d, want 2052", got)
	}
	// An unparseable duration must not stop an episode being published.
	bad := Episode{Duration: "rubbish"}
	if got := bad.DurationSeconds(); got != 0 {
		t.Errorf("DurationSeconds() on bad input = %d, want 0", got)
	}
}
