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
├── pkg/                  config, logger (+betterstack), signaler, version, parquetx, glob, extract, agentkey
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
├── db/<source>/<schema>.<table>/changes/dt=YYYY-MM-DD/<run>-<seq>.parquet
├── _runs/<adapter>/<run>-<seq>.json                               # manifest: files written, counts
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

### 5.3 `postgres` / `mysql` (databases)

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
| `laker views` | DuckDB SQL: `mail`, `mail_messages`, `files`, `files_history`, `files_text`, and per table `db_<source>_<schema>_<table>`, `…_history`, macro `…_at(ts)` |

### 6.3 Web UI

Served by `laker serve` (`common.web`):

- search with highlighting and paging; mail reading (text, HTML version, attachments, `.eml` download);
  file browsing now or at a past date with the history of each file; adapter status with "Run now";
  agents page;
- **access**: a token (≥ 16 characters) asked once, then an `HttpOnly`, `SameSite=Strict` cookie; POSTs must
  come from the same site; default bind `127.0.0.1`, exposed only behind a TLS reverse proxy;
- **mail HTML**: `cid:` images become `data:` URIs, remote images are blocked until asked for (they track
  the reader), then bluemonday removes scripts, styles, forms and handlers; the result goes into a
  `sandbox` iframe without scripts, under a CSP that allows nothing remote;
- **downloads**: only PDF, images, text, audio and video open in the browser; everything else (HTML
  included) is only downloaded, so no file of the lake runs in the page's origin;
- with the web UI enabled, `serve` keeps the state and index open; the CLI commands that need them go
  through its JSON API (`/api/*`, `Authorization: Bearer <token>`).

### 6.4 Why Parquet and not Iceberg

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
schemaversion: 1
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
  web: { enabled: true, host: 127.0.0.1, port: 8090, token: ${LAKER_WEB_TOKEN} }
  agents: { enabled: false }
  defaults: { timeout: 2h, commitevery: 500 }

adapters:
  - name: mail-personal
    enabled: true
    type: imap                       # imap | fs | postgres | mysql
    schedule: "@every 10m"           # 6-field cron with seconds, @every, @hourly, or manual
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

---

## 9. Decisions

1. **Hetzner**: Object Lock is supported; create the bucket with Object Lock and a default retention.
   `serve --check` reports it; `conditionalput: true` sends `If-None-Match: *`.
2. **No client-side encryption**: the bucket stays directly readable.
3. **Derived data is snapshotted to the bucket too**: periodic, append-only snapshots of the index
   (`_index/`) and of the state (`_state/`); `laker restore` fills an empty data directory from the latest.
4. **English only** in the repository.

---

## 10. Roadmap

| Milestone | Content |
|---|---|
| **M0** ✅ | skeleton: CLI, strict config, logger, signaler, append-only lake (`s3`/`fs`), `serve --check` |
| **M1** ✅ | `imap` + Bleve search; `fs` (blobs + inventory) + browsing at a date; `reindex`, `state rebuild`, snapshots |
| **M2** ✅ | `postgres`, `mysql` (snapshot + diff); text extraction; `laker views` |
| **M3** ✅ | web UI in `serve`; CLI through its API |
| **M4** ✅ | `laker` as an always-on service fed by `laker-agent` (§11) |
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
| `POST commit` | the Parquet files and index changes of a commit (`domain.Commit`) |
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
  `files/inventory/source=<s>/`, `files/text/source=<s>/`, `db/<s>/`), index only documents of its source and
  unindex only its own files. A compromised agent can only add to its own sources: the lake stays
  append-only.

### 11.6 Agent configuration

laker: `common.agents.enabled: true` (needs `common.web`, whose listener it uses). The agent
(`laker-agent.yaml`, see §11.7 to write it):

```yaml
schemaversion: 1
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
- **`laker-agent config discover`** looks, read-only and locally, for: IMAP accounts of Thunderbird and Evolution;
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
- CI builds laker for Linux and laker-agent also for macOS and Windows, where data often lives.

### 11.10 Known limits

- A commit travels in one request (up to 1 GiB): with very long mails, keep `commitevery` low.
- The agent's checkpoints live only on the agent: if its `datadir` is lost it reads everything again (blobs
  are not duplicated, Parquet rows are) and must be approved again, since its key is new.
- `laker-agent run` cannot run while `laker-agent serve` does (the state is single-process).
