# Laker

A personal data lake in a single Go binary. Laker copies your **mailboxes**, **files** and small
**databases** into an S3 bucket in open formats, never deletes anything, and lets you **search and
browse** all of it from one web page — including how things were on any past date.

- **Mail** (IMAP): every message kept as its original `.eml`, plus searchable headers and text.
- **Files**: every content stored once (SHA-256), every change recorded; browse a folder as it was
  last month, download any past version. Text of PDF, Office, OpenDocument and HTML files is searchable.
- **Databases** (Postgres, MySQL/MariaDB): latest rows and full history of every row, no
  `updated_at` column needed.
- **Append-only**: nothing in the lake is ever deleted or overwritten; works with S3 Object Lock.
- **Open formats**: originals, Parquet and JSON manifests — the bucket is readable without Laker
  (`laker views` prints DuckDB views over it).
- **Go only**: one static binary, no cgo, no external services.

## How it is deployed

```
 laptop / NAS / office LAN                         cloud or home server
┌──────────────────────────┐   HTTPS, signed    ┌─────────────────────────────┐     ┌──────────────┐
│ laker-agent              │ ─────────────────▶ │ laker serve                 │ ──▶ │ S3 bucket    │
│  imap · files · postgres │   (outbound only)  │  lake · search index · web  │     │ (the lake)   │
└──────────────────────────┘                    └─────────────────────────────┘     └──────────────┘
```

- **`laker serve`** is the always-on service: it owns the lake, the search index and the web UI,
  and can run sources itself.
- **`laker-agent`** runs next to the data with the same sources and sends what they read to laker.
  Credentials stay on the agent and never reach laker; nothing has to reach into your networks. Each agent has an Ed25519
  identity; laker accepts it only after you approve its ID.

A single machine needs only `laker serve`.

## Quick start

```bash
make build                          # bin/laker, bin/laker-agent  (Go 1.27, vendored)

bin/laker config create             # writes config.yaml: set the bucket, the web token, the adapters
export LAKER_S3_ACCESS_KEY=… LAKER_S3_SECRET_KEY=… LAKER_WEB_TOKEN=…
bin/laker serve --check             # reaches the bucket, reports Object Lock, writes nothing
bin/laker serve                     # web UI on http://127.0.0.1:8090
```

An adapter in `config.yaml`:

```yaml
adapters:
  - name: mail-personal
    enabled: true
    type: imap                      # imap | fs | postgres | mysql
    schedule: "@every 10m"          # 6-field cron, @every, @hourly, or manual
    params:
      host: imap.example.com
      username: me@example.com
      password: ${IMAP_PASS}
```

### Adding an agent

```bash
# on laker: common.agents.enabled: true (behind a TLS reverse proxy)
# on the machine with the data:
laker-agent config generate         # laker-agent.yaml: every adapter type, documented, disabled
laker-agent discover                # finds mail accounts, folders, databases; asks before saving a secret
laker-agent config validate         # connects to every enabled adapter
laker-agent serve                   # prints its ID and waits
# on laker:
laker agents approve lkr-xxxxxxxx-xxxxxxxx-xxxxxxxx-xxxxxxxx   # or the "Agents" page
```

### Docker, systemd, Kubernetes

`Dockerfile` builds two images: `docker build --target laker -t laker .` and
`docker build --target laker-agent -t laker-agent .`. See `deploy/` for docker compose, systemd
units and a Kubernetes manifest. Run one replica of laker: its state and index are single-writer files,
derived from the lake and restorable (`laker restore`, or `laker reindex` + `laker state rebuild`).

## Command line

| Command | |
|---|---|
| `laker serve [--check]` | scheduler, web UI, agent API |
| `laker run <adapter> [--dry-run]` | run once now |
| `laker search <query>` | full-text search (`from:`, `subject:`, `ext:`, `kind:mail\|file`, Italian stemming) |
| `laker show <sha>` · `laker get <sha\|source:path> [--at date]` | read a message, download an original |
| `laker ls <source>[:dir] [--at date]` | browse files, now or in the past |
| `laker list` · `laker agents [approve\|revoke <id>]` | adapters and their runs, agents |
| `laker views` | DuckDB SQL over the lake |
| `laker reindex` · `laker state rebuild` · `laker snapshot` · `laker restore` | derived data |
| `laker-agent serve\|id\|run\|list` | the agent |
| `laker-agent config generate\|validate` · `laker-agent discover` | write and check its configuration |

While `laker serve` runs, the CLI goes through its API.

## Bucket layout

```
blobs/sha256/ab/cd/<sha256>                 originals: .eml, files
mail/messages/account=<a>/month=YYYY-MM/    one row per message and folder
files/inventory/source=<s>/dt=YYYY-MM-DD/   every add, modify, move, delete
files/text/source=<s>/dt=YYYY-MM-DD/        text extracted from documents
db/<source>/<schema.table>/changes/dt=…/    row changelog: insert, update, delete
_runs/  _state/  _index/                    manifests, snapshots of the derived data
```

## Documentation

- [`docs/SPEC.md`](docs/SPEC.md) — design, data model, decisions
- [`CLAUDE.md`](CLAUDE.md) — conventions for contributors: architecture, log and error codes

## License

[MIT](LICENSE)
