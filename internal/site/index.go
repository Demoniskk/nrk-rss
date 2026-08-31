package site

// indexTemplate is the static listing page. It is deliberately plain: no build
// step, no framework, no network calls. The podcast list is rendered by Go and
// the search box only hides and shows rows that are already on the page.
const indexTemplate = `<!doctype html>
<html lang="no">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>NRK Podcast RSS</title>
<meta name="description" content="Standard RSS-feeder for alle podkaster på NRK Radio.">
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
    font: inherit; color: var(--text);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .search input:focus { outline: 2px solid var(--accent); outline-offset: 1px; }
  #count { margin: .5rem 0 0; color: var(--muted); font-size: .87rem; }
  ul { list-style: none; margin: 0; padding: 0; display: grid; gap: .6rem; }
  li {
    display: flex; gap: .9rem; align-items: center;
    padding: .8rem .9rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  li[hidden] { display: none; }
  img {
    width: 56px; height: 56px; flex: 0 0 56px;
    border-radius: 8px; object-fit: cover; background: var(--accent-soft);
  }
  .meta { min-width: 0; flex: 1; }
  .title { font-weight: 600; display: block; }
  .sub {
    color: var(--muted); font-size: .85rem;
    display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  }
  .links { display: flex; gap: .4rem; flex-wrap: wrap; flex: 0 0 auto; }
  .links a, .links button {
    font: inherit; font-size: .85rem;
    padding: .35rem .65rem;
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
  var haystacks = items.map(function (li) {
    return (li.getAttribute('data-search') || '').toLowerCase();
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
