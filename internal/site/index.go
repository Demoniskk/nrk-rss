package site

import (
	"html/template"
	"strings"
)

// indexFuncs are the helpers the index template needs.
var indexFuncs = template.FuncMap{
	"summary": summarise,
}

// summaryLimit is how much of a podcast description the listing page shows.
// It is generous enough for the two rendered lines on a wide screen and short
// enough that 200-odd descriptions do not dominate the page weight.
const summaryLimit = 220

// summarise collapses a description onto a single line and trims it to
// summaryLimit runes, cutting on a word boundary. NRK descriptions range from
// one sentence to several paragraphs, so the raw text cannot be trusted to sit
// nicely in a card.
func summarise(s string) string {
	s = strings.Join(strings.Fields(s), " ")

	r := []rune(s)
	if len(r) <= summaryLimit {
		return s
	}
	cut := string(r[:summaryLimit])
	// Only back up to the previous space if that still leaves most of the
	// text; a single very long word should be cut mid-word instead.
	if i := strings.LastIndex(cut, " "); i > len(cut)/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:-") + "…"
}

// indexTemplate is the static listing page. It is deliberately plain: no build
// step, no framework, no network calls. The podcast list is rendered by Go and
// the search box only hides and shows rows that are already on the page.
const indexTemplate = `<!doctype html>
<html lang="no">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NRK Podcast RSS</title>
<meta name="description" content="Standard RSS-feeder for alle podkaster på NRK Radio. Søk opp en podkast, kopier feed-lenken, og lim den inn i podkastspilleren din.">
<meta name="theme-color" content="#fbfbfd" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#0f1014" media="(prefers-color-scheme: dark)">
<style>
  :root {
    color-scheme: light dark;
    --bg: #fbfbfd;
    --surface: #ffffff;
    --border: #e3e3ea;
    --text: #16161d;
    --muted: #6b6b7b;
    --accent: #0b5fff;
    --accent-soft: #eef3ff;
    --radius: 12px;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0f1014;
      --surface: #17181e;
      --border: #2a2b34;
      --text: #ececf2;
      --muted: #9a9aad;
      --accent: #7aa2ff;
      --accent-soft: #1b2233;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: var(--bg);
    color: var(--text);
    font: 16px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    /* Stop iOS inflating text when the phone is rotated. */
    -webkit-text-size-adjust: 100%;
  }
  .wrap { max-width: 940px; margin: 0 auto; padding: 2.5rem 1.25rem 4rem; }
  header h1 { font-size: 1.9rem; margin: 0 0 .4rem; letter-spacing: -.02em; }
  header p { margin: 0 0 .35rem; color: var(--muted); }
  header p a { color: var(--accent); }
  .stats { font-variant-numeric: tabular-nums; }
  .search {
    position: sticky; top: 0; z-index: 2;
    padding: 1.25rem 0 1rem;
    background: linear-gradient(var(--bg) 78%, transparent);
  }
  .search input {
    width: 100%; padding: .7rem .9rem;
    /* Inherits 16px. Anything smaller makes iOS Safari zoom in on focus,
       which leaves the page scrolled sideways. */
    font: inherit; color: var(--text);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .search input:focus { outline: 2px solid var(--accent); outline-offset: 1px; }
  #count { margin: .5rem 0 0; color: var(--muted); font-size: .87rem; }
  ul { list-style: none; margin: 0; padding: 0; display: grid; gap: .6rem; }
  li {
    display: grid;
    grid-template-columns: 56px minmax(0, 1fr) auto;
    grid-template-areas: "art meta links";
    gap: .9rem; align-items: start;
    padding: .85rem .9rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  li[hidden] { display: none; }
  img {
    grid-area: art;
    width: 56px; height: 56px;
    border-radius: 8px; object-fit: cover; background: var(--accent-soft);
  }
  .meta { grid-area: meta; min-width: 0; }
  .title { font-weight: 600; display: block; overflow-wrap: anywhere; }
  .sub {
    color: var(--muted); font-size: .85rem;
    display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .desc {
    margin: .3rem 0 0;
    color: var(--muted); font-size: .87rem; line-height: 1.4;
    overflow-wrap: anywhere;
    /* Two lines, then ellipsis. Browsers that ignore line-clamp just show the
       whole summary, which is already trimmed server-side. */
    display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .links { grid-area: links; display: flex; gap: .4rem; flex-wrap: wrap; }
  .links a, .links button {
    font: inherit; font-size: .85rem;
    display: inline-flex; align-items: center; justify-content: center;
    /* 40px tall so the three actions are comfortable targets on a phone. */
    min-height: 40px; padding: .35rem .7rem;
    border-radius: 8px; border: 1px solid var(--border);
    background: transparent; color: var(--accent);
    text-decoration: none; cursor: pointer;
  }
  .links a.primary { background: var(--accent-soft); border-color: transparent; }
  .links a:hover, .links button:hover { border-color: var(--accent); }
  footer { margin-top: 2.5rem; color: var(--muted); font-size: .85rem; }
  footer a { color: var(--accent); }
  .empty { padding: 2rem 0; color: var(--muted); }
  .failures { margin-top: 2rem; font-size: .87rem; color: var(--muted); }
  .failures summary { cursor: pointer; }
  .failures code { word-break: break-all; }

  /* Phones: the three action buttons no longer fit beside the text, so they
     move to their own full-width row under the artwork and title. */
  @media (max-width: 34rem) {
    .wrap { padding: 1.75rem 1rem 3rem; }
    header h1 { font-size: 1.5rem; }
    li {
      grid-template-columns: 48px minmax(0, 1fr);
      grid-template-areas: "art meta" "links links";
      gap: .75rem;
    }
    img { width: 48px; height: 48px; }
    .links a, .links button { flex: 1 1 0; }
  }
</style>
</head>
<body>
<div class="wrap">

<header>
  <h1>NRK Podcast RSS</h1>
  <p>Standard RSS-feeder for podkastene på NRK Radio — abonner i den podkastspilleren du vil.</p>
  <p class="stats">{{ .PodcastCount }} podkaster · {{ .EpisodeCount }} episoder{{ if .LatestEpisode }} · nyeste episode {{ .LatestEpisode }}{{ end }}</p>
</header>

<div class="search">
  <label for="q" hidden>Søk etter podkast</label>
  <input id="q" type="search" placeholder="Søk etter podkast…" autocomplete="off" spellcheck="false">
  <p id="count" hidden></p>
</div>

<ul id="list">
{{- range .Podcasts }}
  <li data-search="{{ .Title }} {{ .ID }} {{ .Category }}">
    {{- if .ImageURL }}
    <img src="{{ .ImageURL }}" alt="" loading="lazy" width="56" height="56">
    {{- else }}
    <img alt="" width="56" height="56">
    {{- end }}
    <div class="meta">
      <span class="title">{{ .Title }}</span>
      <span class="sub">{{ .EpisodeCount }} episoder{{ if .Category }} · {{ .Category }}{{ end }}</span>
      {{- if .Description }}
      <p class="desc">{{ summary .Description }}</p>
      {{- end }}
    </div>
    <div class="links">
      <a class="primary" href="feeds/{{ .ID }}.xml">RSS</a>
      <button type="button" class="copy" data-url="{{ .FeedURL }}">Kopier</button>
      <a href="{{ .NRKURL }}" rel="noopener">NRK</a>
    </div>
  </li>
{{- end }}
</ul>

<p class="empty" id="empty" hidden>Ingen podkaster matcher søket.</p>

{{- if .Failures }}
<details class="failures">
  <summary>{{ len .Failures }} podkast(er) feilet under siste kjøring</summary>
  <ul>
  {{- range .Failures }}
    <li><code>{{ .ID }}</code>: {{ .Error }}</li>
  {{- end }}
  </ul>
</details>
{{- end }}

<footer>
  <p>
    Feedene lenker til lydfiler NRK selv publiserer; ingen lyd lastes ned eller re-hostes her.
    Maskinlesbar oversikt: <a href="feeds.json">feeds.json</a>.
  </p>
</footer>

</div>
<script>
(function () {
  var input = document.getElementById('q');
  var list = document.getElementById('list');
  var empty = document.getElementById('empty');
  var count = document.getElementById('count');
  var items = Array.prototype.slice.call(list.getElementsByTagName('li'));

  // Precompute the haystack once; filtering then stays cheap while typing.
  // The description is read back off the rendered element rather than repeated
  // in an attribute, so searching it costs no extra bytes in the page.
  var haystacks = items.map(function (li) {
    var desc = li.getElementsByClassName('desc')[0];
    var text = (li.getAttribute('data-search') || '') + ' ' + (desc ? desc.textContent : '');
    return text.toLowerCase();
  });

  function filter() {
    var q = input.value.trim().toLowerCase();
    var shown = 0;
    for (var i = 0; i < items.length; i++) {
      var match = q === '' || haystacks[i].indexOf(q) !== -1;
      items[i].hidden = !match;
      if (match) shown++;
    }
    empty.hidden = shown !== 0;
    count.hidden = q === '';
    count.textContent = shown + ' treff';
  }

  input.addEventListener('input', filter);
  filter();

  list.addEventListener('click', function (ev) {
    var btn = ev.target.closest ? ev.target.closest('button.copy') : null;
    if (!btn || !navigator.clipboard) return;

    // The feed URL is absolute when the site was built with --base-url, and
    // relative otherwise. Resolving against the current page gives a URL that
    // is pasteable into a podcast app either way.
    var url = new URL(btn.getAttribute('data-url'), location.href).href;
    navigator.clipboard.writeText(url).then(function () {
      var original = btn.textContent;
      btn.textContent = 'Kopiert!';
      setTimeout(function () { btn.textContent = original; }, 1200);
    });
  });
})();
</script>
</body>
</html>
`
