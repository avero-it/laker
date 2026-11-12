# Laker — Specification

> A **personal data lake** in Go. Laker ingests mailboxes (IMAP), files and small databases (Postgres,
> MySQL/MariaDB) into an S3 bucket (Hetzner Object Storage) in open, **immutable** formats, and offers
> **full-text search and browsing** over all of it. `laker serve` is the always-on service that owns the
> lake; `laker-agent` runs next to the data and sends it there.

---

## 1. Scope and rules

**What it does**
1. **Mail**: fetches several IMAP mailboxes, keeping the original `.eml`, the metadata in Parquet and a
   full-text index.
2. **Files**: copies them into the lake (a few hundred GB), deduplicated by content, with the history of
   every version; search and browse the folder tree, also as it was on a past date.
3. **Databases**: small Postgres and MySQL/MariaDB databases; latest version and full history of every row.

**Rules that do not bend**
- **Nothing is ever deleted from the lake**: no delete, overwrite, retention or compaction. Deletions at a
  source become new records (`op=delete`).
- **Open formats**: the bucket is readable without Laker (`.eml`, original files, Parquet → DuckDB).
- **Go only**: no external runtime, no cgo, no helper service (Tika, Elasticsearch, Python).
- **Everything outside the bucket is derived and rebuildable**: search index, local state.
- **English only** in the repository.

**What it is not** (so as not to rebuild Airbyte)
- Not an ELT platform: no connector marketplace, no configuration UI, no generic transforms, no
  "staged/gold" models.
- A source is added only when it is really needed.
- No Iceberg for now (§6.4).

---

## 2. Stack

| Area | Choice |
|---|---|
| Language / module | Go 1.27.3, `github.com/avero-it/laker`, `vendor/` committed |
| CLI / config / log | `spf13/cobra`, `spf13/viper` (YAML, strict decoding), `rs/zerolog` + `fatih/color` + Betterstack writer |
| Schedule | `robfig/cron/v3` (seconds, `WithLocation`) |
| S3 | `aws-sdk-go-v2` (Hetzner endpoint, path style) |
| Parquet | `parquet-go/parquet-go` |
| IMAP / MIME | `emersion/go-imap/v2`, `emersion/go-message` |
| Databases | `jackc/pgx/v5`, `go-sql-driver/mysql` |
| Search | `blevesearch/bleve/v2` (Italian and English analysis of every text) |
| Local state | `go.etcd.io/bbolt` |
| Text extraction | `klippa-app/go-pdfium` (WebAssembly on wazero, no cgo), `archive/zip` + `encoding/xml` (DOCX/PPTX/ODF), `xuri/excelize` (XLSX), `golang.org/x/net/html` |
| Web UI | standard library (`net/http`, `html/template`), `microcosm-cc/bluemonday` (mail HTML sanitizing) |
| Agent identity | `crypto/ed25519` |
| Tests | standard library; real IMAP server in memory (`go-imap/v2/imapserver`); Postgres/MariaDB when available |

---

## 3. Hexagonal architecture

```
               cmd/laker (service)                      cmd/laker-agent (agent)
                     │                                          │
   in   core: Runner (source side) · LocalIngest (lake side) · Scheduler · catalog · rebuild · snapshot
                     │
   out  Source · RunSink · Ingest · Lake · StateStore · Indexer
                     │
   adapters   sources/{imap,fs,sqldb}   lake/{s3,fs}   state/bbolt   indexer/bleve
              web (UI + /api)           agentapi (laker side of /agent/v1)   agentclient (agent side)
```

```
laker/
├── cmd/laker/            the service: CLI, wiring, serve --check, agents
├── cmd/laker-agent/      the agent
├── intern/core/          runner, LocalIngest, scheduler, catalog, rebuild, snapshot, agents, browse
│   ├── domain/           rows (MailRow, FileRow, TextRow, DB changelog), index docs, state keys, lake keys
│   └── ports/out/        Lake, Source + RunSink, Ingest, StateStore, Indexer
├── intern/adapters/      lake, sources (+ registry), indexer, state, web, agentapi, agentclient
├── pkg/                  config, logger (+betterstack), signaler, version, parquetx, glob, extract, agentkey,
│                         redact, heartbeat
└── deploy/  docs/  Dockerfile  makefile  vendor/
```

### 3.1 Main ports

```go
// Source reads what is new at one origin and hands it to the RunSink of the run.
type Source interface {
    Run(ctx context.Context, rs RunSink) error
}

// RunSink: blobs go to the Ingest at once (idempotent by content); rows, index documents and
// checkpoint changes are staged and land together at Commit, the checkpoint last.
type RunSink interface {
    parquetx.Tables                      // typed rows → Parquet
    RunID() string
    PutBlob(ctx, sha256 string, size int64, r io.Reader) (created bool, err error)
    IndexMail(domain.MailDoc); IndexFile(domain.FileDoc); UnindexFile(id string)
    GetState(key string) ([]byte, error); ForEachState(prefix string, fn …) error
    SetState(key string, value []byte);   DeleteState(key string)
    Commit(ctx) error
}

// Ingest is where a run delivers: in-process for laker's own adapters (core.LocalIngest),
// over HTTPS for an agent (agentclient.Client).
type Ingest interface {
    BlobExists(ctx, sha256 string) (bool, error)
    PutBlob(ctx, sha256 string, size int64, r io.Reader) (created bool, err error)
    Commit(ctx, domain.Commit) error      // parquet files + index changes
    Finish(ctx, domain.Run) error         // run record
}

// Lake has no Delete, by contract.
type Lake interface {
    PutIfAbsent(ctx, key string, r io.Reader, size int64) (created bool, err error)
    Exists(ctx, key string) (bool, error)
    Get(ctx, key string) (io.ReadCloser, error)
    List(ctx, prefix string, fn func(key string) error) error
    Check(ctx) (domain.LakeInfo, error)   // reaches the lake, writes nothing
}
```

On S3, `PutIfAbsent` sends `If-None-Match: *` (`conditionalput: true`) after a `HEAD`: keys are
deterministic (content hashes, run IDs), so a second write of the same key is harmless.

### 3.2 A run

1. The scheduler fires; if the previous run of the same adapter is still going, this one is skipped.
2. The source reads its checkpoint, finds what is new, puts the blobs (no-op if the hash exists).
3. It buffers Parquet rows and index documents; every `commitevery` items, and at the end, it commits:
   the Ingest writes the Parquet files and the manifest, updates the index, **then** the checkpoint is
   saved (at-least-once).
4. Every run has a timeout and a propagated `context`; on SIGTERM the commit in progress completes.
5. A failed run saves no checkpoint: the next one redoes only the unconfirmed part (at worst duplicated
   Parquet rows, never duplicated blobs).

---

## 4. Bucket layout

```
s3://<bucket>/
├── blobs/sha256/ab/cd/abcd…ef                                    # .eml and original files, one per content
├── mail/messages/account=<a>/month=YYYY-MM/<run>-<seq>.parquet
├── files/inventory/source=<s>/dt=YYYY-MM-DD/<run>-<seq>.parquet
├── files/text/source=<s>/dt=YYYY-MM-DD/<run>-<seq>.parquet      # text extracted from documents
├── chat/messages/source=<s>/month=YYYY-MM/<run>-<seq>.parquet    # Telegram and WhatsApp messages
├── db/<source>/<schema>.<table>/changes/dt=YYYY-MM-DD/<run>-<seq>.parquet
├── _runs/<adapter>/<run>-<seq>[-p<part>].json                     # manifest: files written, counts
├── _index/<ts>.tar.gz                                             # snapshot of the Bleve index
└── _state/<ts>.db                                                 # snapshot of the bbolt state
```

- `<run>` is a ULID (time-ordered); names are unique, so no file is ever rewritten.
- Typed Parquet tables carry a `field_id` on every column, zstd compression and min/max statistics.

---

## 5. Sources

### 5.1 `imap` (mail)

- **Connection**: TLS (or STARTTLS, or none for tests), password or app password. Folders selected by
  include/exclude patterns (`*`, `**`; every other character literal, so `[Gmail]/All Mail` works). For
  Gmail, use only `[Gmail]/All Mail` to avoid duplicates from labels.
- **Incremental**: per folder `UIDVALIDITY` + last `UID`. If `UIDVALIDITY` changes the folder is read
  again: the blobs exist already, only new occurrence rows are written.
- **First run**: everything, or from `since`.
- **Per message**: the raw RFC 5322 message → `sha256` → blob; a row in `mail/messages`; an index document.
- **Row**: `sha256, account, mailbox, folder, uidvalidity, uid, message_id, thread_key, in_reply_to, date,
  from, to[], cc[], subject, body_text, has_html, attachments[] {name, mime, size}, flags[], size,
  internal_date, seen_at, run_id, note`.
- **Identity**: a message is its `sha256`; `(account, folder, uidvalidity, uid)` is one occurrence. The search
  shows a message once, with all the places it is in.
- Messages over `maxsize` (default 100 MB) get a row but no blob (`note: too large`).
- Not tracked yet: deletions and flag changes on the server.

### 5.2 `fs` (files)

- Scans the configured directories (mount a NAS), with exclude patterns, not following symlinks.
- **Change detection**: against the inventory in the state (`path → size, mtime, sha256`); the hash is
  computed again only when size or mtime changed.
- **New content**: files up to 64 MB are hashed and uploaded from memory; larger ones from a local copy in
  `datadir/tmp`, so the uploaded bytes are exactly the hashed ones even if the file changes meanwhile.
- **Inventory row** (changes only): `source, path, op (add|modify|delete|move), from_path, sha256, size,
  mtime, mode, seen_at, run_id`. `move` = same content at a new path in the same scan.
- An unreadable root marks nothing as deleted.
- **Text**: PDF, DOCX, PPTX, ODT/ODS/ODP, XLSX, HTML and text files (UTF-8 or Latin-1) up to
  `extractmaxsize` (50 MB) are extracted for the search and stored in `files/text/` once per content and
  source. No OCR: a scanned PDF gives no text.
- **Browsing at a date**: last event per path with `seen_at <= T`, without deletes.

### 5.3 `telegram` (private chats and groups)

- The user API (MTProto, `gotd/td`, pure Go): the Bot API cannot read a user's history. Each user registers
  an `api_id`/`api_hash` at my.telegram.org (none ships with Laker).
- **Login**: `laker-agent telegram login <adapter>` asks for the code Telegram sends and the two-step
  password; the session (full access to the account) is saved in `<datadir>/telegram-<adapter>.session`,
  mode 0600, and never leaves the machine. It shows in Telegram's active sessions as "laker-agent".
- **Read**: private chats (users, bots, Saved Messages) and groups (basic and super); not broadcast channels,
  not secret chats (they exist on one device only). Title patterns in `chats`/`exclude`.
- **First import** of a chat inside an export (takeout) session, as Telegram Desktop's "Export chat
  history": lower limits, but Telegram asks to confirm it in the app and may delay it about 24 hours for a
  new session; meanwhile runs end with that message and retry at the next schedule. History is read oldest
  first, 100 messages a request, committed every `commitevery`: an interrupted import resumes.
- **Later runs**: the messages after the last one stored, and the last `lookback` (7 days) again to catch
  edits, which become `update` rows. Deletions are not seen (no always-on connection).
- `FLOOD_WAIT` answers are waited out (up to 15 minutes, 10 times) and requests are rate-limited.
- **Media** (photos, videos, voice notes, audio, documents) up to `mediamaxsize` (50 MB) and of the selected
  `mediatypes` are stored as blobs; the others are recorded with a note, like large mails.
- **Row** (`chat/messages/source=<s>/month=YYYY-MM/`): `service, chat_id, chat_title, chat_type, msg_id,
  date, edit_date, op, from_id, from_name, text, reply_to, forwarded_from, media[], seen_at, run_id,
  chat_key, msg_key, reply_key, from_key`. Chat ids follow the Bot API: users > 0, groups < 0. The `*_key`
  columns hold the ids as text for every service (WhatsApp ids are not numbers); rows written before they
  existed have them empty, and the `chat_history`/`chats` views fill them in.
- The web UI shows chat hits and the conversation around a message (`/chat/<source>/<chat>`).

### 5.4 `whatsapp` (private chats and groups)

- WhatsApp has no API to read one's own chats: the agent is a **linked device** of the phone, as WhatsApp
  Web, through `go.mau.fi/whatsmeow` (pure Go, MPL-2.0). It is not an official client: WhatsApp could
  restrict the account; Laker only reads and never sends.
- **Login**: `laker-agent whatsapp login <adapter>` shows a QR code to scan (WhatsApp > Linked devices), or
  with `--phone` a code to type on the phone. The session (access to the account) is a SQLite database
  (`ncruces/go-sqlite3`, WebAssembly, no cgo) in `<datadir>/whatsapp-<adapter>.db`, mode 0600; it never
  leaves the machine. The phone lists it as "laker-agent".
- **History**: the phone sends it once, right after pairing (full sync asked for, `historydays` days; the
  phone decides how much). There is no way to ask again later but to pair again.
- **Every run** connects, receives what the server kept for this device since the last run (new messages,
  edits, deletions) until it says it is done and nothing comes for 30 s, or `wait` (10 min), then
  disconnects. WhatsApp unlinks a device offline for about two weeks: schedule it at least every few days.
- **Nothing received is lost**: an event is written to a local spool (`<session>.spool/`, fsync) before it
  is acknowledged to the server, and removed from the spool only once committed to the lake. History blobs
  are downloaded from the spool; one that fails stays there for the next run.
- **Edits** become `update` rows, **deletions** `delete` rows carrying the last content: the message stays
  in the lake and in the index, marked deleted. The source keeps the last version of every message in its
  state (`msg:<chat>:<id>`), so a message delivered twice is written once; `laker state rebuild` restores it.
- **Read**: private chats and groups; not status updates, channels, broadcast lists, reactions, calls.
  Names come from the phone's contacts and group subjects; patterns in `chats`/`exclude`.
- **Media** as for Telegram (`mediamaxsize`, `mediatypes`, plus `sticker`). WhatsApp's servers keep media
  for a limited time: old history media may no longer be downloadable and are recorded with a note.

### 5.5 `postgres` / `mysql` (databases)

**Snapshot and diff**: no `updated_at` column needed, deletions are seen.

1. One connection opens a consistent read-only snapshot (Postgres `REPEATABLE READ READ ONLY`,
   MySQL/MariaDB `START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY`) and reads every selected table in it.
2. For each row: the primary key (or, without one, the row hash plus a counter for identical rows) and the
   hash of the canonical encoding of names, types and values.
3. Compared with the hashes of the previous run (`row:<table>\0<pk>` in the state) → `insert`, `update`,
   `delete`.
4. Only the changes are written to `db/<source>/<table>/changes`, with typed columns plus `_op, _pk,
   _row_hash, _seen_at, _schema_hash, _run_id`. Deleted rows keep their key values.

- **Latest version** = last row per `_pk` without `_op='delete'`; **history** = the changelog;
  **as of T** = the same with `_seen_at <= T`.
- **Types**: integers → INT64, floats → DOUBLE, boolean → BOOLEAN, date/timestamp → TIMESTAMP(µs, UTC),
  bytea/blob → BINARY, json/jsonb → JSON, `numeric`/`decimal` and everything else → text (no value is
  altered). In MySQL `boolean` is `tinyint(1)` and arrives as an integer.
- **Schema changes** produce files with the new schema; read them with `union_by_name=true`.
- **Granularity**: one version per row per run; a row changed twice between runs shows only the last change.
- MySQL reads only the database named in the DSN unless `tables` says otherwise.

---

## 6. Search, browsing, formats

### 6.1 Index (Bleve)

- Local (`<datadir>/index`), derived from the bucket: `laker reindex` rebuilds it from the Parquet files.
- **Italian and English**, often mixed in one message: every text field is indexed twice, with an Italian
  analyzer under its name and an English one under `<name>_en` (only the first is stored), and every query
  is rewritten to ask both, so `fattura` finds `fatture` and `invoice` finds `invoices`. Both analyzers split
  on every non-alphanumeric character (so `contract.txt` and `anna.smith@example.com` are found word by
  word) and fold accents (`perche` finds `perché`); Italian adds elision, stop words and light stemming,
  English stop words and Snowball stemming. No language detection is needed.
- The index records the version of its mapping; an index built by an older laker is refused with a message
  asking for `laker reindex`. Matches found only through the English twin show their fragment unhighlighted.
- **Mail documents**: `subject, from, to, cc, body, attachments, account, folder, date, size`.
- **File documents**: `path, name, ext, size, mtime, source, content`.
- **Queries**: Bleve query-string syntax: free words, `field:value`, `+must`, `-not`, `"phrases"`.

### 6.2 Command line

| Command | |
|---|---|
| `laker search <query> [--kind mail\|file] [--limit N] [--json]` | full-text search |
| `laker show <sha256>` | a message (headers and text) or where a file content is |
| `laker get <sha256\|source:path> [--at date] [-o file]` | download an original |
| `laker ls <source>[:dir] [--at date]` | browse files, now or in the past |
| `laker verify [--hash N\|--hash-all] [--json]` | check the lake against its manifests and rows, reading only (§9.6) |
| `laker views` | DuckDB SQL: `mail`, `mail_messages`, `files`, `files_history`, `files_text`, and per table `db_<source>_<schema>_<table>`, `…_history`, macro `…_at(ts)` |

### 6.3 Web UI

Served by `laker serve` (`common.web`):

- search with highlighting and paging; mail reading (text, HTML version, attachments, `.eml` download);
  file browsing now or at a past date with the history of each file; adapter status with "Run now";
  agents page; connectors page (§6.4);
- **access**: a token (≥ 16 characters) asked once, then an `HttpOnly`, `SameSite=Strict` cookie; POSTs must
  come from the same site; default bind `127.0.0.1`, exposed only behind a TLS reverse proxy;
- **mail HTML**: `cid:` images become `data:` URIs, remote images are blocked until asked for (they track
  the reader), then bluemonday removes scripts, styles, forms and handlers; the result goes into a
  `sandbox` iframe without scripts, under a CSP that allows nothing remote;
- **downloads**: only PDF, images, text, audio and video open in the browser; everything else (HTML
  included) is only downloaded, so no file of the lake runs in the page's origin;
- with the web UI enabled, `serve` keeps the state and index open; the CLI commands that need them go
  through its JSON API (`/api/*`, `Authorization: Bearer <token>`).

### 6.4 MCP for Claude connectors

With `common.mcp.enabled`, `serve` also answers MCP (Streamable HTTP, stateless, JSON responses) at
`<common.web.publicurl>/mcp`, so a Claude custom connector (claude.ai, desktop, mobile, and the dashboards
that call connectors) can read the lake. It runs on the stores the web UI already holds open.

- **read-only tools**: `search`, `get_document`, `list_files` (now or at a date), `file_history`, `read_file`
  (extracted text), `status`; no run, no write, no agent action. Long text is cut at 60,000 characters
  (Claude takes about 150,000 per result, which carries the output twice). No SQL over Parquet yet: DuckDB
  needs cgo.
- **OAuth 2.1, Laker as its own authorization server** (`intern/adapters/oauth`): protected resource
  metadata (RFC 9728, also announced in the `WWW-Authenticate` of every 401), authorization server metadata
  (RFC 8414), dynamic client registration (RFC 7591), authorization code with PKCE `S256` only, the
  `resource` of RFC 8707 checked against `/mcp`, `iss` in the redirect (RFC 9207), revocation (RFC 7009).
- **who gets in**: anyone may register a client, but only with a redirect URI of `common.mcp.redirecturis`
  (default Claude's, `https://claude.ai/api/mcp/auth_callback`); nothing is granted until the owner approves
  it on the consent page by typing the web token (failures wait 1 s and are rate-limited). The web token
  itself never opens `/mcp`.
- **tokens**: opaque, 32 random bytes; access 1 hour, refresh 60 days and rotated at every use; only
  their SHA-256 is stored, in `datadir/oauth.db`. That file is not derived from the lake, so it is neither
  in the snapshots nor touched by `state rebuild`: losing it only means approving the clients again.
  The Connectors page lists the clients and revokes them (with every token they hold).
- **network**: Claude calls from Anthropic's servers (`160.79.104.0/21`), never from the user's device, so
  the listener must be reachable over public HTTPS behind the reverse proxy (TLS there); restricting
  `/mcp`, `/oauth/` and `/.well-known/` to that range at the proxy is a sensible extra.
- **what leaves**: whatever a tool returns (mail bodies included) goes to Claude, as any connector's data.

To connect: Claude → Settings → Connectors → Add custom connector → `https://<host>/mcp`, then approve
on the page that opens.

### 6.5 Why Parquet and not Iceberg

The lake is append-only and small, so Iceberg's strengths (row deletes, MERGE, safe compaction, planning
over millions of files) are not needed; time travel already comes from the changelogs. The files are ready
for Iceberg (`field_id`, stable schemas, date partitions): if needed, an `iceberg` sink can register them as
they are (add-files), without rewriting them, with the maintenance operations that delete files disabled.

---

## 7. Configuration

`/etc/laker/config.yaml` or `./config.yaml` (`--config` overrides). Decoding is strict (an unknown key is an
error), `${VAR}` in any value is read from the environment, `laker config validate` reports every problem
at once. `laker config create` writes a commented example.

```yaml
schemaversion: 3
common:
  id: { name: laker }
  timezone: Europe/Rome
  datadir: /var/lib/laker            # local state, index, scratch files: derived
  log: { verbose: false, use: console, betterstack: { sourcetoken: "", ingestinghost: "" } }
  lake:
    use: s3                          # s3 | fs (development)
    s3:
      endpoint: https://fsn1.your-objectstorage.com
      region: fsn1
      bucket: my-lake
      accesskey: ${LAKER_S3_ACCESS_KEY}
      secretkey: ${LAKER_S3_SECRET_KEY}
      pathstyle: true
      conditionalput: true
    fs: { path: ./lake }
  snapshot: { schedule: "0 30 4 * * *" }
  web: { enabled: true, host: 127.0.0.1, port: 8090, token: ${LAKER_WEB_TOKEN}, publicurl: https://laker.example.com }
  mcp: { enabled: false, redirecturis: [] }   # §6.4; empty: Claude's callback
  agents: { enabled: false }
  defaults: { timeout: 2h, commitevery: 500 }

adapters:
  - name: mail-personal
    enabled: true
    type: imap                       # imap | fs | postgres | mysql
    schedule: "@every 10m"           # 6-field cron with seconds, @every, @hourly, or manual
    heartbeat: ${MAIL_HEARTBEAT_URL} # optional: uptime monitor told how every run ended (§8.1)
    params: { host: imap.example.com, username: me@example.com, password: ${IMAP_PASS}, folders: ["*"] }
  - name: files-home
    enabled: true
    type: fs
    schedule: "0 0 3 * * *"
    params: { paths: [/home/me/Documents], exclude: ["**/.git/**"] }
  - name: db-erp
    enabled: true
    type: postgres
    schedule: "0 */30 * * * *"
    params: { dsn: ${ERP_DSN}, tables: ["public.*"] }
```

`laker serve --check` runs the init phase, reaches the bucket (reporting Object Lock and retention) and
prints `LAKER_CHECK_RESULT {json}`, writing nothing.

---

## 8. Logging

- Output: `console` (colored), `betterstack` (console + asynchronous JSON batches), `stdout-json`.
- One Betterstack writer per process; component loggers derive from it and it is flushed on shutdown.
- Colored prefixes: `[LAKER]`/`[CORE]` white, `[SCHED]` yellow, `[SRC:<name>]` cyan, `[LAKE]` green,
  `[INDEX]`/`[AGENTS]` magenta, `[WEB]` blue, infrastructure grey.
- Never mail or file contents, nor credentials, in the logs; the web server logs paths, not queries.
- Unique codes (prefix = level, error values always `ERR`, three hex digits per package block);
  `TestErrorCodesAreUnique` and `TestEveryLogLineHasACode` enforce them.

### 8.1 Heartbeats

A source that stops being read loses data silently (WhatsApp unlinks a device offline for two weeks), and
logs do not say that nothing happened. An adapter, of laker or of an agent, may have a `heartbeat`: the
address of an uptime monitor (a Better Stack heartbeat, healthchecks.io) that `pkg/heartbeat` calls after
every run that is not a dry run: as it is when the run succeeded, with `/fail` appended when it failed.
The monitor, set to the period of the schedule plus some grace, then warns when the source fails, when it
stops running and when the whole process is down. Nothing about the run is sent. The address holds a
token: it is never logged nor quoted in an error, and belongs in a `${VARIABLE}`. A monitor that cannot be
reached is a warning (`WRN104`), never a failed run.

---

## 9. Decisions

1. **Hetzner**: Object Lock is supported; create the bucket with Object Lock and a default retention.
   `serve --check` reports it; `conditionalput: true` sends `If-None-Match: *`.
2. **No client-side encryption**: the bucket stays directly readable.
3. **Derived data is snapshotted to the bucket too**: periodic, append-only snapshots of the index
   (`_index/`) and of the state (`_state/`); `laker restore` fills an empty data directory from the latest.
   A snapshot stays in the bucket for ever, so one is taken only if a commit reached the lake since the
   last (or after `laker reindex`); checkpoints and run records alone do not count.
4. **The rebuilds are bounded in memory**: `laker reindex` and `laker state rebuild` read the Parquet files
   one at a time, in the order the runs wrote them (`<run>-<seq>`: run ids are time-ordered and the runs of
   a source never overlap), and write in batches. They keep only what must be compared across files: where
   each message is, the latest tree of each source, the live rows of each database table. `state rebuild`
   writes into a scratch file that replaces the state only when complete.
5. **English only** in the repository.
6. **The lake can be checked without Laker's derived data**: `laker verify` reads only the bucket and
   writes nothing. It checks that every file of every manifest is there, that every Parquet file can be
   read, that every content named by a mail, file or chat row is there, and that contents still hash to
   their names (100 picked at random by default, `--hash-all` for all: that reads the whole lake). It
   exits 1 if anything is missing or damaged. Parquet files of no manifest and contents no row names are
   what an interrupted run leaves: reported, not errors. It reads what refers before what is referred to,
   so it can run while `laker serve` ingests.

---

## 10. Roadmap

| Milestone | Content |
|---|---|
| **M0** ✅ | skeleton: CLI, strict config, logger, signaler, append-only lake (`s3`/`fs`), `serve --check` |
| **M1** ✅ | `imap` + Bleve search; `fs` (blobs + inventory) + browsing at a date; `reindex`, `state rebuild`, snapshots |
| **M2** ✅ | `postgres`, `mysql` (snapshot + diff); text extraction; `laker views` |
| **M3** ✅ | web UI in `serve`; CLI through its API |
| **M4** ✅ | `laker` as an always-on service fed by `laker-agent` (§11) |
| **M5** | read-only MCP behind OAuth for Claude connectors (§6.4) |
| later, if needed | IMAP deletions/flags, text of mail attachments, search over database rows, CDC, Iceberg, more sources |

---

## 11. laker + laker-agent

### 11.1 Why

`laker serve` runs where the lake is (a cloud VM, a home server, a Kubernetes pod) and stays on; the data
is elsewhere: the laptop, the NAS, the office database reachable only from the LAN. `laker-agent` runs
**next to the data**, reads it with the same sources (IMAP, files, Postgres/MySQL) and **sends** it to
laker, which reaches nothing itself: the agents connect out to it. Advantages:

- mailbox and database credentials stay on the agent's machine;
- no port open towards the data networks, only outbound HTTPS to laker;
- the lake, the index and the web UI have one place and one writer;
- several agents (home, office, NAS) feed the same lake and the same search.

`laker serve` may still run adapters of its own; the two coexist.

### 11.2 Division of work

| Source side (agent, or laker for its adapters) | Lake side (always laker) |
|---|---|
| reads, hashes, extracts document text | checks and writes the blobs |
| keeps its checkpoints in its own `state.db` | writes the Parquet files and the run manifest |
| encodes rows as Parquet, prepares index documents | updates the index (merging mail locations) |
| saves a checkpoint only once laker confirmed | records the run (status, `list`, web UI) |

Every operation is idempotent (blobs by hash, files by run ID): a repeated request changes nothing, so the
client retries network errors and 5xx answers, never refusals.

A commit larger than about 32 MB on the wire is sent **in pieces** (`Commit.Split`): the Parquet files
first, then the index documents, each piece a commit of its own with the same run and sequence and a
`part` number; a single file or document larger than that travels alone. Each piece after the first has
its own manifest (`_runs/<adapter>/<run>-<seq>-p<part>.json`). laker reads at most two commit bodies at a
time, the others wait, so several agents committing together do not add up in its memory.

### 11.3 Identity and approval

- On its first start the agent creates an **Ed25519** key (`<datadir>/agent.key`, mode 0600). Its **ID** is
  derived from the public key: `lkr-` + 160 bits of the SHA-256 in base32, e.g.
  `lkr-kp4ocmfu-kiejvyi3-7wp2rc35-rtzdrwfm`. It cannot be guessed, nor used without the private key.
- `laker-agent serve` prints the ID and presents itself (`POST /agent/v1/hello`, signed with the key it
  presents). laker records it as **pending**; the agent asks again every 30 seconds.
- Approve it on laker with `laker agents approve <id>` or the **Agents** page, checking the ID against the
  one the agent printed. `laker agents revoke <id>` refuses every further request.

### 11.4 Protocol (`/agent/v1/*`, on the web listener)

| Request | Effect |
|---|---|
| `POST hello` | registers or refreshes the agent, answers `{id, status}` |
| `HEAD blobs/{sha}` | 200 if the content is in the lake, 404 otherwise |
| `PUT blobs/{sha}` | laker receives into a temporary file, **hashes it again** and stores it only if it matches |
| `POST commit` | the Parquet files and index changes of a commit (`domain.Commit`), or one piece of it |
| `POST finish` | the run record |

Every request carries `X-Laker-Agent`, `X-Laker-Time`, `X-Laker-Content-SHA256` and `X-Laker-Signature`, the
Ed25519 signature of `laker-agent-v1 / method / path / time / body hash`. laker refuses wrong signatures,
times more than 5 minutes off its clock and agents that are not approved. Confidentiality comes from TLS:
laker belongs behind an HTTPS reverse proxy (the agent warns about a plain `http://` remote server).

### 11.5 Who may write what

- Each **source** (adapter name) has one writer (`_owners`): the first agent that uses it, or `laker serve`
  for its own adapters. Another agent with the same name gets `409`, so names must be unique (convention:
  the agent as prefix, `home-mail`, `nas-documents`).
- A commit may write only under its source's prefixes (`mail/messages/account=<s>/`,
  `files/inventory/source=<s>/`, `files/text/source=<s>/`, `db/<s>/`, `chat/messages/source=<s>/`), index only
  documents of its source and
  unindex only its own files. A compromised agent can only add to its own sources: the lake stays
  append-only.

### 11.6 Agent configuration

laker: `common.agents.enabled: true` (needs `common.web`, whose listener it uses). The agent
(`laker-agent.yaml`, see §11.7 to write it):

```yaml
schemaversion: 2
agent:
  name: home
  server: https://laker.example.com
  timezone: Europe/Rome
  datadir: /var/lib/laker-agent     # key, checkpoints, scratch files
  log: { verbose: false, use: console, betterstack: { sourcetoken: "", ingestinghost: "" } }
  defaults: { timeout: 2h, commitevery: 200 }
adapters:                           # as in laker
  - { name: home-documents, enabled: true, type: fs, schedule: "0 0 3 * * *", params: { paths: [/home/me/Documents] } }
```

Agent commands: `serve`, `id`, `run <adapter> [--dry-run]`, `list`, `config generate|discover|validate`,
`version`.

### 11.7 Writing the agent configuration

The configuration is the hard part for a single user or a small company, so the agent helps with it:

- **`laker-agent config generate`** writes a complete file: the agent section guessed for this machine
  (name from the hostname, local time zone, data directory of the OS) and one documented, disabled adapter
  of every type, generated from the sources themselves (`sources.Stubs`), so it never drifts from the code.
  `--discover` adds what `config discover` finds, disabled.
- **`laker-agent config validate`** checks the form, then **probes every enabled adapter**: it logs in to
  the mailboxes and lists the selected folders, reads the directories (bounded count of files and bytes),
  connects to the databases, opens the read-only snapshot a run would use and lists the selected tables.
  `--offline` checks the form only; `--json` for scripts. The variables of disabled adapters need not be set.
- **`laker-agent config discover`** looks, read-only and locally, for: IMAP accounts of Thunderbird, Evolution,
  Claws Mail, Geary, KMail, mutt/neomutt, OfflineIMAP and mbsync/isync (host, port, security and user name only,
  never the password, even when a client keeps it in clear text);
  the document folders (XDG names, so localized ones too); mounted network shares; Postgres (`.pgpass`,
  `PG*` variables, a server on 5432) and MySQL/MariaDB (`.my.cnf`, a server on 3306); Postgres, MySQL and
  MariaDB containers through the Docker socket, with the credentials of their environment. On a terminal
  it proposes each source in turn; **each credential it found is shown masked and saved only if confirmed**,
  a missing one can be typed (hidden) or left as a `${VARIABLE}`; the adapter is probed at once and
  enabled if the probe passes. Without a terminal it adds everything disabled and saves no secret.
  Discovery runs only on demand. No LLM is involved: an assistant may come later, fed only with what
  discover found, never with secrets.
- **Secrets** live in `laker-agent.env` next to the configuration (mode 0600, refused if others can read
  it), read by laker-agent before the configuration; variables already in the environment win.

### 11.8 Credentials never leave the agent

The credentials of an agent's sources live only on the agent's machine (`laker-agent.yaml`,
`laker-agent.env`, the environment). laker receives only blobs, Parquet files, index documents and run
records. Run errors are scrubbed of anything that looks like a credential (URL passwords, MySQL DSN
passwords, `password=` pairs) before they are recorded, logged or sent (`pkg/redact`); failed runs are
reported to laker too, so its status page shows why. `config discover` and `config validate` send nothing.

### 11.9 Deployment

- **Two images** from one `Dockerfile` (targets `laker` and `laker-agent`, distroless, non-root), each with
  its entrypoint and tags; the data machines pull only the agent.
- `deploy/docker-compose.yml`, systemd units (`deploy/laker.service`, `deploy/laker-agent.service`),
  `deploy/k8s/laker.yaml` (one-replica StatefulSet with a volume for state and index, Service).
- One replica of laker: state and index are single-writer files. They are derived: a new node starts with
  `laker restore` (snapshot) or `laker reindex` + `laker state rebuild`.
- CI (`build.yml`) runs the tests and cross-compiles laker for Linux and macOS and laker-agent also for
  Windows, where data often lives (laker itself needs POSIX file access).
- **Releases** (`.goreleaser.yaml`, `.github/workflows/release.yml`): a tag `vX.Y.Z` builds static archives,
  `.deb`/`.rpm`/`.apk` packages (binary, systemd unit, system user and directories, example config; the
  service is never started automatically and data is never removed), the GitHub release, Homebrew
  formulas with a `service` block (`avero-it/homebrew-tap`) and a Scoop manifest for the agent
  (`avero-it/scoop-bucket`). The tap and bucket are written with the `TAP_GITHUB_TOKEN` secret; without it
  that step is skipped. `apt.yml` then rebuilds a GPG-signed apt repository from the `.deb` of every
  release (`scripts/build-apt-repo.sh`, key in `APT_GPG_PRIVATE_KEY`) and publishes it on GitHub Pages.
  The version is stamped with `-ldflags -X .../pkg/version.version`.

### 11.10 Known limits

- The agent still builds a whole commit in memory before cutting it in pieces: with very long mails or
  documents, keep `commitevery` low. laker accepts single requests up to 1 GiB, for older agents.
- Update laker before its agents: an older laker gives every piece of a commit the manifest name of the
  first, so only that one is written (the data and the index are complete all the same).
- The agent's checkpoints live only on the agent: if its `datadir` is lost it reads everything again (blobs
  are not duplicated, Parquet rows are) and must be approved again, since its key is new.
- `laker-agent run` cannot run while `laker-agent serve` does (the state is single-process).
