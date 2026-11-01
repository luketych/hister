---
date: '2026-04-07T11:00:00+00:00'
draft: false
title: 'Extractors'
description: 'Understand and configure the built in content handlers used for indexing and previews.'
---

<script>
  import ConfigReference from '#lib/ConfigReference.svelte';

  const ytdlpOptions = [
    {
      name: 'binary',
      type: 'string',
      defaultValue: 'yt-dlp',
      description: 'Path to the yt-dlp executable. Use this when the binary is not on PATH or to select a specific version.',
    },
    {
      name: 'timeout',
      type: 'int',
      defaultValue: '15',
      description: 'Seconds to wait for yt-dlp to finish before aborting the request.',
    },
    {
      name: 'max_concurrent_jobs',
      type: 'int',
      defaultValue: '2',
      description: 'Maximum number of yt-dlp processes running at once. Additional jobs wait for a slot. Set it to zero for no limit.',
    },
    {
      name: 'fetch_subtitles',
      type: 'bool',
      defaultValue: 'false',
      description: 'Downloads subtitles or an automatically generated transcript and appends them to the indexed text and preview.',
    },
    {
      name: 'sub_language',
      type: 'string',
      defaultValue: 'auto',
      description: 'Subtitle language code to request. Auto selects automatically generated captions. Used only when fetch_subtitles is true.',
    },
    {
      name: 'cookies_file',
      type: 'string',
      defaultValue: '(none)',
      description: 'Path to a Netscape format cookies file passed to yt-dlp. Useful for restricted content.',
    },
    {
      name: 'cookies_from_browser',
      type: 'string',
      defaultValue: '(none)',
      description: 'Browser from which yt-dlp extracts cookies. This takes precedence over cookies_file.',
    },
    {
      name: 'extra_args',
      type: 'string[]',
      defaultValue: '(none)',
      description: 'Additional yt-dlp command line flags appended verbatim to every invocation.',
    },
    {
      name: 'extra_domains',
      type: 'string[]',
      defaultValue: '(none)',
      description: 'Additional domains to match this extractor. They should take the form "domain.tld", e.g. "vimeo.com". Check the yt-dlp repository for a full list of supported sites.'
    }
  ];

  const ytDownloaderOptions = [
    {
      name: 'binary',
      type: 'string',
      defaultValue: 'yt_downloader',
      description: 'Executable path or command name resolved through PATH.',
    },
    {
      name: 'output_dir',
      type: 'string',
      defaultValue: '~/Videos/Hister/YouTube',
      description: 'Directory where downloaded video and audio files are stored.',
    },
    {
      name: 'timeout',
      type: 'int',
      defaultValue: '1800',
      description: 'Maximum time in seconds for downloading both video and audio files.'
    },
    {
      name: 'max_concurrent_jobs',
      type: 'int',
      defaultValue: '2',
      description: 'Maximum number of background download attempts running at once. Zero uses the default two workers.',
    },
    {
      name: 'backend',
      type: 'string',
      defaultValue: 'auto',
      description: 'Downloader backend: auto, yt-dlp, or youtube-dl.',
    },
    {
      name: 'transcribe',
      type: 'boolean',
      defaultValue: 'false',
      description: 'Download available English manual or automatic captions as VTT files. This does not run speech recognition or add captions to Hister indexed text.',
    },
  ];
</script>

Extractors are the components responsible for turning raw HTML or file content
into rich, searchable data. Specialist extractors understand particular sites
and file formats. General extractors handle everything else.

## Purpose

Generic HTML-to-text conversion loses a lot of signal. A Stack Overflow answer,
a Go package reference, a local Markdown note, and a news article all have
different structure and a one-size-fits-all parser cannot take advantage of
that structure.

Extractors exist so that each kind of source can be handled in the most
**domain-specific** way possible. A specialist extractor for a particular
website or file format can:

- pull out the parts of the page that actually matter and discard noise (ads,
  navigation, boilerplate)
- produce richer plain text that makes search results more relevant
- surface structured details answers, code snippets, documentation sections
  that a generic parser would flatten or miss entirely
- give each content type a suitable preview layout and presentation

The goal is always to capture **more specialised, higher-quality information**
about the content being processed, so that search results and the document
preview are as useful as possible for the source in question.

When a page is fetched by the browser extension, the CLI, or the crawler,
Hister processes its raw HTML or file contents into searchable text and a
preview.

## Configuration

Each extractor can be enabled or disabled, and may expose custom options,
through the `extractors` section of the config file.

```yaml
extractors:
  <extractor-name>:
    enable: true | false
    options:
      key: value
```

Use the lower case extractor names listed below as configuration keys.

Only entries you want to change from the default need to be specified. If an
extractor is omitted from the config, its built-in defaults apply.

### `enable`

Controls whether the extractor participates in the chain.

| Value   | Effect                                                                                                 |
| ------- | ------------------------------------------------------------------------------------------------------ |
| `true`  | Extractor is active. The external-tool extractors `ytdlp` and `yt_downloader` are disabled by default. |
| `false` | Extractor is skipped for automatic indexing and preview selection                                      |

### `options`

A map of settings supported by that extractor. An unrecognised option causes a
configuration error.

## Built-in extractors

The extractors below are listed in processing order. Specialist handlers run
before general fallbacks. Metadata enrichers can add details without replacing
the searchable body.

### `markdown`

Provides sanitized HTML previews for locally indexed Markdown files. The file
indexer renders `.md` and `.markdown` source into HTML before the extractor
chain runs, so this extractor leaves indexed text unchanged and handles the
preview.

**Matches:** `file://` URLs ending in `.md` or `.markdown`.

### `orgmode`

Provides sanitized HTML previews for locally indexed Org mode files. The file
indexer renders `.org` source into HTML before the extractor chain runs, so this
extractor leaves indexed text unchanged and handles the preview.

**Matches:** `file://` URLs ending in `.org`.

### `embeddedvideo`

Scans `iframe`, `video`, `embed`, and `object` elements for embedded video URLs.
Discovered entries are stored in the document's `videos` metadata without
replacing its searchable body or preview.

**Matches:** pages whose raw HTML contains a supported embedding element.

### `jsonld`

Parses every `<script type="application/ld+json">` block in the page and writes
normalised [schema.org](https://schema.org) metadata to the document. It
captures the `@type` content classification and `headline` fields. General HTML
extraction still supplies the searchable body text.

**Matches:** any page that contains the `application/ld+json` substring.

### `reddit`

Extracts one Reddit post together with every comment already present in the
page. Indexed text and the preview preserve reply depth. Current Reddit pages
are read through semantic custom elements and content slots. Legacy pages are
read through Reddit thing identifiers, with schema.org discussion data as a
fallback.

The extractor does not request or expand hidden comments. Subreddit listings,
profiles, search results, and every other Reddit page are excluded.

**Matches:** Reddit post paths containing `/comments/{post_id}` and short post
links on `redd.it`.

### `discourse`

Extracts one Discourse topic together with every regular post already present
in the page. It combines rendered `article[data-post-id]` content with the
embedded topic stream, which preserves post numbers, authors, timestamps,
reply targets, likes, reactions, and accepted solutions. Schema.org `QAPage`
and crawler markup provide stable fallbacks.

The extractor performs no additional requests. Hidden posts and system action
entries are excluded. Category lists, tag lists, profiles, search pages, and
topic API formats are rejected.

**Matches:** topic paths such as `/t/{slug}/{topic_id}` on pages carrying a
Discourse generator, setup, or environment marker.

### `stackexchange`

Extracts Stack Exchange network question pages, including Stack Overflow,
Server Fault, Super User, Ask Ubuntu, MathOverflow, Stack Apps, and
`*.stackexchange.com` communities. Indexed text includes the question body and
all answers, with accepted answers marked.

The preview pane shows the full question body followed by each answer separated
by a horizontal rule, with accepted answers marked.

**Matches:**

- [stackoverflow.com](https://stackoverflow.com)
- [serverfault.com](https://serverfault.com)
- [superuser.com](https://superuser.com)
- [askubuntu.com](https://askubuntu.com)
- [mathoverflow.net](https://mathoverflow.net)
- [stackapps.com](https://stackapps.com)
- [stackexchange.com](https://stackexchange.com)

### `godoc`

Provides a rich preview for Go package documentation. The preview pane renders
the `Documentation-content` section of the page with relative links rewritten to
absolute `pkg.go.dev` URLs. General HTML extraction supplies the searchable
text.

**Matches:** `https://pkg.go.dev/…`

### `github`

Extracts searchable content and previews from GitHub repository roots, issue
pages, issue lists, and pull request pages. Repository results include the
description, star count, topics, programming languages, and README. Issue and
pull request results include their page specific metadata and discussion
content.

Issue previews show the status, labels, description, and comments with their
authors and dates when available. Formatting, code blocks, links, and task
completion are preserved. The preview uses the saved page, so comments that
were not loaded when the page was indexed are not included. Open the original
issue to see the latest discussion.

**Matches:** `https://github.com/{owner}/{repo}`, its `/issues` list, individual
`/issues/{number}` pages, and individual `/pull/{number}` pages. GitHub system
paths such as `/settings`, `/topics`, and `/explore` are excluded.

### `lobsters`

Extracts the full content of a lobste.rs submission, including the story
metadata (title, author, tags, submission date), the optional story body, and
the complete nested comment tree. Both indexed text and preview preserve the
parent–child comment hierarchy.

**Matches:** `https://lobste.rs/s/…`

### `hackernews`

Extracts the full content of a Hacker News item page, including the submission
metadata (title, target site, score, author, submission date), the optional self
text of an Ask HN or text post, and the complete comment tree. Hacker News
renders comments as one flat table and records depth in the `indent` attribute
of each row, so both the indexed text and the preview rebuild the parent–child
hierarchy from it.

**Matches:** `https://news.ycombinator.com/item?id=…`

### `wikipedia`

Extracts article content from Wikipedia. Indexed text includes the article
title, infobox key–value pairs, and the body text with navigation boxes,
references, and other noise removed. The preview renders the article HTML with
inline styles applied, videos replaced by their poster frames, and relative URLs
rewritten to absolute Wikipedia URLs.

**Matches:** `https://*.wikipedia.org/wiki/…` (article pages only; non-content
namespaces such as `Special:`, `Talk:`, `User:`, `File:`, and `Category:` are
excluded).

### `mastodon`

Handles Mastodon instance pages by splitting them into individual toot documents.
Each toot found on the page is indexed as a separate document with its own URL
and author, allowing individual posts to appear in search results. The original
aggregator page is not indexed. Links for remote toots are rewritten to point
directly to the account's original server instead of the instance displaying
the federated copy.

Detection is heuristic: the extractor checks for a `"repository":"mastodon/mastodon"`
marker in the page HTML, or for a `type: toot` metadata flag set by a previous
pass.

**Matches:** any Mastodon instance page containing the Mastodon source marker.

### `bluesky`

Handles Bluesky profiles, custom feeds, and post threads by splitting every
available post into a separate document. Each document receives its canonical
`bsky.app` URL, author, handle, publication time, and a
`type: bluesky` metadata marker. The source page itself is not indexed.

The extractor prefers schema.org `DiscussionForumPosting` and `Comment` data
published by Bluesky. This supports profile and individual post pages even
without browser rendering. It also reads rendered feed and thread cards through
semantic test identifiers, roles, post links, and content attributes. Generated
CSS class names are not used. Rich rendered content is preferred when both
representations describe the same post, while canonical URL deduplication keeps
one document per post.

Custom feeds require the `chromedp` or `bidi` crawler backend because their post
lists are rendered in the browser. Profile and individual post pages also work
with the `http` backend through their JSON LD data.

**Matches:** profile, feed, and post pages on `bsky.app`, `www.bsky.app`, and
`embed.bsky.app`.

### `twitter`

Handles Twitter and X feeds, profiles, and tweet pages by splitting every
visible tweet into a separate document. Each tweet receives its canonical X
URL, author, handle, publication time, and a `type: tweet` metadata marker. The
feed or conversation page itself is not indexed.

The extractor first reads schema.org `SocialMediaPosting` metadata. It also
supports rendered tweet elements and Open Graph metadata from individual tweet
pages. This allows it to handle both server rendered pages and pages captured
after the application has loaded in a browser.

When a rendered tweet body is available, it is preferred over semantic metadata
so original link destinations produced by the application are preserved.
Expanded link attributes on anchors inside the tweet body replace `t.co`
aliases in indexed text. When an expanded attribute is absent, a complete URL
rendered as the anchor text is used instead. Truncated visible URLs are not
guessed. Preview anchors point directly to the original destination. A `t.co`
URL with no available destination remains plain text rather than becoming a
clickable redirect link.

**Matches:** feeds, profiles, and tweet pages on `twitter.com` and `x.com`,
including their common web and mobile hostnames.

### `notion`

Extracts the title and rendered block content of Notion pages and produces a
sanitized preview. Notion renders content in the browser, so indexing requires
the `chromedp` or `bidi` crawler backend. Hister does not index the empty
application shell when the rendered block content is missing.

**Matches:** nonroot pages on `notion.so`, `www.notion.so`, and any
`*.notion.site` domain.

<span id="yt_video_downloader"></span>

### `yt_downloader`

`extractors.yt_video_downloader` remains a configuration alias. New configuration,
extractor lists and preview-menu entries use `yt_downloader`; saved legacy preview
selections still resolve to the same single extractor. Conflicting overlapping
values under both names in one configuration layer are rejected instead of
choosing a winner. Matching or disjoint values combine into one entry. Normal
file/managed/override precedence still applies across layers under either name.
Reading or reloading a legacy configuration does not rewrite it, move media,
or rename the existing cache/report directory.

A local download enricher and preview renderer for this Hister checkout. It
invokes the configured `yt_downloader` executable in video and audio-only
modes. The audio mode does not promise a particular container or stream
composition; the downloader may use its documented audio-bearing media fallback.
By default Hister does not request captions. Set `transcribe: true` to request
available English captions as VTT sidecars; this does not run speech recognition
or add captions to Hister's indexed text. The enricher stages a server-owned
download request; work is accepted only after the source document commits.
Background workers download independently, so normal content extraction and
searchable indexing do not wait for media to finish. A pair becomes ready only
after both files pass manifest/file validation and atomic registry publication.

Select this extractor in the preview menu to see **In queue**, **Starting**,
**Downloading**, **Downloaded — waiting for audio**, **Finalizing**,
and terminal failure/interruption states. The preview polls the read-only
`/api/yt_downloader/status?document_id=...` endpoint while work is active;
closing it stops polling, not the accepted download. Playback links appear
only for committed artifacts whose files are available. Missing and retired
files have no playback links and are not described as queued. A working older
download remains playable while a refresh is queued or fails. No separate
search results are created. Normal content extractors (such as `ytdlp`) still
provide the usual content preview. The media output directory does not need
to be configured under `indexer.directories`.

Each invocation gets a unique job ID and media subdirectory under `output_dir`.
Hister accepts paths only from the invocation's completed JSON result manifest;
it does not guess from downloader logs or scan the output directory. The
configured executable must implement the named-job flags and **result JSON
schema v1** (`--job-id`, `--output-root`, `--artifacts-root`, and
`--result-json`). Older or incompatible downloader builds fail the background
attempt with an upgrade hint in the server log; normal indexing and the previous
selection are unaffected. Operational reports and transcripts are not playable
artifacts. Submitted/imported metadata cannot grant access to any media file.

New links use `/api/yt_downloader/media?id=...`. IDs are not bearer credentials:
every request checks source visibility and the server-owned binding. Legacy
`/api/yt_video_downloader/media?document_id=...&uri=...` links resolve only through
verified retained bindings, never metadata. Successful refreshes and index
rebuilds retain previously issued links and their original files. Historical
files predating the trusted registry are **not automatically authorized**;
explicit verified administrative reconciliation is still required.

For an authorized live binding, unavailable media links return a safe JSON
error rather than playable content: **404 `media_missing`** for a missing file,
**404 `media_unavailable`** when opening or file validation fails, and
**410 `media_retired`** for an intentionally retired artifact. A registry read
failure after source authorization returns **503 `media_registry_unavailable`**.
Unknown, foreign, revoked and suspended bindings remain indistinguishable plain
404s, including for old links. Diagnostic responses are not cached; HEAD omits
the diagnostic body. Logs for these diagnostics contain only bounded codes and
server-owned artifact IDs, not file paths, legacy URIs or raw errors. Reading
an unavailable link does not schedule a replacement; status reports separately
whether real work is active.

Fresh unprocessed page submissions request a refresh. Processed imports,
`Save`, index rebuilds and status/preview reads do not request downloads.
Compatible in-flight requests for the same source/owner/options reuse the real
attempt; a later fresh submission after it finishes requests another attempt.
Hister retains media and failed-attempt files. It removes temporary result
manifests, not media directories; per-job reports remain in the local cache.
Deletion revokes bindings without deleting bytes, and re-add does not revive
old links. Ownership moves cancel incompatible work and transfer retained
history; a partial SQL/index move remains unavailable until deliberately
reconciled. Cleanup and legacy reconciliation are not automatic.

The extractor is **disabled by default**. Enable it only after installing a
compatible `yt_downloader` build. This extractor is local to this custom Hister
build and is not included in Hister's published binary. Shutdown interrupts
claimed work; queued work resumes after restart when the extractor is enabled.
If disabled on restart, unresolved work is marked interrupted rather than shown
as queued without a dispatcher. Cancelling accepted work or deleting its source
fences publication and terminates the downloader process group on Linux and
macOS; other platforms may not stop child processes. Merely closing an indexing
request or preview does not cancel already accepted work.

**Matches:** supported video URLs on `youtube.com`, `youtube-nocookie.com`, and
`youtu.be`, including watch, shorts, live, embed, and short-link forms. Playlist,
channel, and other pages without an unambiguous video ID are not downloaded.

#### Historical evidence inventory (dry-run only)

Use the local CLI to inspect explicitly selected historical Hister job IDs:

```bash
hister media reconcile \
  --state-dir /path/to/hister/yt_video_downloader \
  --media-root /path/to/media \
  hister-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-video \
  hister-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-audio
```

Replace the example IDs with known report jobs. Both directories must already
exist. The state directory is the retained `yt_video_downloader` cache directory,
not its `artifacts` child. The command reads only the fixed
`artifacts/<job>/jobs/<job>/request.json`, `result.json` and `manifest.json`
reports and their named provenance/media files. It does not scan filenames,
titles, modification times or unrelated downloads to guess associations.

This command **always performs a dry run** and has no apply or owner-mapping
option. It skips runtime configuration and logging initialization, never opens
SQL/Bleve, contacts a server or downloader, registers artifacts, schedules work,
or moves/deletes files. Explicit roots are required rather than guessed from
configuration. JSON output is versioned, deterministic and includes relative
candidate paths, bounded issue codes and a digest of the inspected JSON evidence
(not a media-content hash or signature). A successful exit means the inventory
was produced, not that all selected jobs passed inspection.

Consistent request/result/manifest/provenance identities, modes, paths and file
sizes still yield **`owner_approval_required`**, never ready or authorized.
Unsigned reports cannot prove which Hister user initiated a job; report fields
claiming ownership are ignored. Sources with missing roles are marked
`incomplete_pair`; multiple possible jobs are `ambiguous_pair`. Even a single
video/audio combination requires `pair_approval_required`, not an inferred
pairing or current selection. Incomplete, mismatched, unsafe or unsupported
reports are blocked. Unknown historical formats are not silently upgraded.

The inventory does not check the live source document, its owner/incarnation,
or existing registry registrations. It cannot authorize media or repair
Greenpath or any other historical playback links. Retain the files and evidence;
do not treat this inventory JSON as an authorization manifest.

#### Explicit source-incarnation bootstrap

Historical approvals require a live registry source ID. For an already-indexed
YouTube document from before the registry existed, first use the admin-only
bootstrap check to verify the exact indexed document ID, owner and URL. The check
is read-only; it does not infer the owner, create rows, or change the selection.
Use the exact returned confirmation in a separately confirmed apply:

```bash
hister media bootstrap-check \
  --document-id '<exact indexed document ID>' \
  --owner-id 0 \
  --url 'https://www.youtube.com/watch?v=abcdefghijk'

hister media bootstrap-apply \
  --document-id '<exact indexed document ID>' \
  --owner-id 0 \
  --url 'https://www.youtube.com/watch?v=abcdefghijk' \
  --confirmation <confirmation_sha256> --yes
```

For a user-owned document, pass its actual indexed `owner_id` instead of zero.
Both endpoints are administrator-only. The server verifies the exact URL, owner,
document ID and YouTube match against its live index. Apply rejects a stale check;
it creates only a new empty source incarnation and returns its `source_id`. It
does not register historical files, create attempts, or revive a revoked
incarnation. If a source already exists, check/apply report its existing ID
idempotently. Put that returned source ID in the reviewed approval below, then
run `hister media check` and the guarded `hister media apply` workflow. Do not
bootstrap or import live history without a consistent backup and explicit
operator approval.

#### Historical approval document and guarded registration

An explicitly reviewed approval maps one exact video/audio pair to an **existing
live source incarnation**. It cannot establish a missing source, infer its owner
from reports, replace the current selection, or start a download. This is the
version-1 shape; placeholders below are deliberately not valid digests:

```json
{
  "schema_version": 1,
  "approval_id": "reviewed-pair-unique-id",
  "action": "retain_history",
  "source": {
    "source_id": "CURRENT_REGISTRY_SOURCE_ID",
    "document_id": "https://www.youtube.com/watch?v=abcdefghijk",
    "owner_id": 0,
    "url": "https://www.youtube.com/watch?v=abcdefghijk"
  },
  "video": {
    "job_id": "hister-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-video",
    "evidence_sha256": "<64 lowercase hex digits from the reviewed inventory>",
    "media_sha256": "<64 lowercase hex digits for the reviewed video bytes>"
  },
  "audio": {
    "job_id": "hister-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-audio",
    "evidence_sha256": "<64 lowercase hex digits from the reviewed inventory>",
    "media_sha256": "<64 lowercase hex digits for the reviewed audio bytes>"
  }
}
```

- `owner_id` must be explicit, including zero for a shared source. Omitted/null
  ownership is rejected. For a nonzero owner, `document_id` is the normal
  `<owner-id>:<exact-source-url>` identity. The registry source ID pins the
  incarnation; deletion/re-add cannot silently reuse an old approval.
- Field names are exact and case-sensitive. Duplicate, case-variant, unknown,
  oversized and multi-value JSON documents are rejected. Only `retain_history`
  is supported; `approval_id` uses 1–120 ASCII letters, digits, hyphens or
  underscores. No actor, executable, filesystem path or downloader option comes
  from approval JSON. During check/apply the server uses its configured media
  output directory and its fixed local `yt_video_downloader` state directory;
  clients cannot choose filesystem roots.
- The approval is **not an administrator credential**. The internal indexer
  service separately requires a verified admin actor (or trusted local admin
  authority in single-user mode) and rechecks the actual indexed source/owner
  and live, unsuspended registry binding under the source commit coordinator.
- Revalidation reads the named reports again and compares their evidence digests.
  Both actual media files are safely opened and hashed against `media_sha256`,
  rejecting even same-size replacements. Descriptor identity, size and mtime are
  rechecked at the commit boundary. Hashing runs outside the short source lock.
- One SQL transaction records the immutable approval/audit actor, historical set,
  artifacts and bookkeeping attempt. It never changes the selected set or latest
  download status. The approval ID is a durable idempotency key: exact replay
  returns the original IDs; changed approval/binding/locations conflict. Previously
  registered jobs cannot be adopted, and retired artifacts are never revived.
  Replays still require current binding/evidence checks; unavailable files may
  therefore fail safely even when an audit receipt already exists.

Use the configured server URL and an administrator token in multi-user mode.
First run a read-only server preflight, review the returned confirmation hash,
then explicitly apply that exact plan:

```bash
hister media check --approval reviewed-history.json
hister media apply --approval reviewed-history.json \
  --confirmation <confirmation_sha256> --yes
```

`media check` validates the live source, ownership, evidence/media hashes and
registry conflicts without creating SQL rows. `media apply` repeats those
checks and requires the exact 64-character hash from the check plus `--yes`.
The hash is a stale-plan guard, **not** a reservation or credential; another
writer may win before apply. Exact replay is harmless and reports the original
receipt. Keep the server media and cache roots under trusted operator control.
SQL and media files do not share a transaction: descriptor checks do not lock
out external filesystem writers, and serving does not re-hash approval bytes on
every read. Tests use synthetic disposable fixtures; no live historical import
is performed by the test suite.

#### Retry and cancel commands

These POST APIs use the same authentication and CSRF protections as other
write APIs. In multi-user mode, only the source owner or an administrator may
mutate its downloads. Shared read access is insufficient; only administrators
may mutate shared user-zero sources. Public read-only servers cannot accept
commands. Submission ownership headers cannot change a command's scope.

Read the status endpoint for `document_id` and `attempt.id`, then send only:

```json
{ "document_id": "<source document ID>", "attempt_id": "<attempt ID>" }
```

- **`POST /api/yt_downloader/retry`** retries the latest failed, cancelled or
  interrupted attempt on an existing committed source, without re-indexing its
  content or replacing the previous ready pair. Current server downloader
  settings and the caller's current URL rules apply; a saved manual skip-rule
  override is preserved. Disabled downloaders and incomplete ownership moves
  cannot start work. Executable paths, options and ownership cannot come from
  the command body or the failed job's old settings.
- **`POST /api/yt_downloader/cancel`** cancels exactly the named queued,
  starting, downloading or finalizing attempt. Publication is fenced before
  signalling its process group. Repeating that cancellation is harmless and
  cannot cancel a later retry. Already-published artifacts are not revoked.

Retry returns **202** for a new accepted intent, or **200** for its existing
idempotent receipt. The old terminal attempt ID is the idempotency key: repeated
requests return the same actual successor, including its later terminal state,
without starting more work. Compatible active work may be reused. To retry a
successor that subsequently fails, use that successor's ID from status, not its
parent's ID. Cancel returns **200** after its cancellation is committed.

Responses contain only `attempt_id` and `state`, plus `replayed` for retry.
The attempt state is not proof that files remain available; use status for
playback availability.
Invalid/stale transitions or already-won publication return **409**; current
URL-rule exclusions return **406**. Unknown/out-of-scope attempts return **404**,
and unavailable dispatch/storage returns **503**. Both commands retain existing
ready files and historical links. Status/preview reads still never enqueue work;
these APIs do not add preview action buttons.

#### Options

<ConfigReference items={ytDownloaderOptions} />

#### Example configuration

```yaml
extractors:
  yt_downloader:
    enable: true
    options:
      binary: yt_downloader
      output_dir: ~/Videos/Hister/YouTube
      timeout: 1800
      backend: auto
      transcribe: false # Set true to download available captions as VTT files.
```

### `ytdlp`

Extracts video metadata from video-hosting sites (YouTube, Vimeo, Twitch, etc.)
using the [`yt-dlp`](https://github.com/yt-dlp/yt-dlp) command-line tool.
Provides a dedicated video preview template that shows the thumbnail, duration,
uploader, description, chapter list, and optional transcript.

The extractor is **disabled by default** because it requires `yt-dlp` to be
installed separately.

**Matches:** a curated list of video-hosting domains (YouTube, Vimeo, Twitch,
Dailymotion, Bilibili, and others), as well as any hostname containing common
video-platform substrings. Additional domains can be matched using the `extra_domains` option.

#### Options

<ConfigReference items={ytdlpOptions} />

#### Example configuration

```yaml
extractors:
  ytdlp:
    enable: true
    options:
      binary: /usr/local/bin/yt-dlp
      timeout: 30
      max_concurrent_jobs: 2
      fetch_subtitles: true
      sub_language: en
      cookies_from_browser: firefox
      extra_args:
        - --proxy
        - socks5://127.0.0.1:1080
      extra_domains:
        - vimeo.com
```

### `chatgpt`

Extracts a ChatGPT conversation as one document. Each
visible user and assistant turn is retained in order and labelled in both the
indexed text and the preview. Headings, lists, tables, code blocks, and ordinary
safe links are preserved in the sanitized preview. System, tool, internal, and
unsupported media content is ignored.

**Matches:** `https://chatgpt.com/c/<conversation-id>` authenticated,
`https://chatgpt.com/share/<conversation-id>` public shared, and
`https://chatgpt.com/g/<gpt-id>/c/<conversation-id>` custom GPT conversation URLs
(including `www.chatgpt.com`).

### `readability`

Generic article extractor using the
[go-readability](https://codeberg.org/readeck/go-readability) library. Strips
navigation, ads, sidebars, and other boilerplate and returns the main article
content as clean plain text and HTML. Also extracts author, publication date,
description, site name, and canonical image from JSON-LD, OpenGraph, and meta
tags.

**Matches:** every page. Acts as the primary fallback for all content that no
specialist extractor handles.

### `basic`

Ultimate fallback. Walks the raw HTML token stream and collects all visible text
inside `<body>`, discarding `<script>`, `<style>`, and `<noscript>` elements.
Produces plain text with no further processing.

**Matches:** every page. Only reached when `Readability` fails or is disabled.
