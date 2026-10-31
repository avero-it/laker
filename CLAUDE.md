# CLAUDE.md — Laker

Laker is a personal data lake in a single Go binary: it ingests mailboxes (IMAP), files and small
databases (Postgres, MySQL) into an append-only S3 bucket (Hetzner Object Storage) in open formats,
and makes them searchable and browsable. The full design is in `docs/SPEC.md`; read it before
adding anything.

## Rules that do not bend

- **English only in the repository**: code, comments, logs, UI text, docs, config examples, tests
  and commit messages. No Italian anywhere (the only exception is test data that exercises the
  Italian search analyzer, marked as such).

- **Nothing is ever deleted or overwritten in the lake.** `out.Lake` has no `Delete` and writes go
  through `PutIfAbsent`. Deletions at a source become new records (`op=delete`). Do not add code
  that removes or replaces lake objects, not even "temporary" ones.
- **Go only**: no cgo, no external services (Tika, Elasticsearch), no Python.
- **Open formats**: the bucket must stay readable without Laker (`.eml`, original files, Parquet).
- **Everything outside the bucket is derived** (local state, search index) and can be rebuilt from it.
- **Not an ELT platform**: add a source only when it is needed; no generic transforms, no connector zoo.

## Layout

```
cmd/laker/            the service: cobra CLI, wiring, serve --check, agents
cmd/laker-agent/      the agent: runs sources near the data, sends them to laker serve
intern/core/          runner (source side) + LocalIngest (lake side), scheduler, catalog, rebuild, snapshot, agents
intern/core/domain/   rows (MailRow, FileRow), index docs, state keys, lake keys
intern/core/ports/out Lake, Source + RunSink, Ingest, StateStore, Indexer
intern/adapters/      lake/{s3,fs}, sources/{imap,fs,sqldb} (+ registry), indexer/bleve, state/bbolt,
                      web (UI + /api), agentapi (laker side of /agent/v1), agentclient (agent side)
intern/coretest/      a real Runner on temp dirs, for tests
pkg/                  config (viper, strict; laker and agent), logger (+betterstack), signaler, version,
                      parquetx, glob, extract, agentkey (Ed25519 identity, request signatures)
```

laker + laker-agent: see `docs/SPEC.md` §11. An agent runs the same sources with
`agentclient` as its `out.Ingest`; laker validates every commit (`core.ValidateCommit`: a
source writes only under its own prefixes) and re-hashes every blob it receives.

Hexagonal: core depends on `ports/out` interfaces; adapters implement them; `cmd/laker` wires them.
Add compile-time guards: `var _ out.Lake = (*Lake)(nil)`.

## Commands

```bash
make test            # go test ./...
make build           # bin/laker
make vendor          # go mod tidy && go mod vendor (vendor/ is committed)
laker config create  # example config.yaml
laker config validate
laker serve --check  # prints LAKER_CHECK_RESULT {json}; exit 0 ok, 1 fails
laker serve
laker run <adapter> [--dry-run]
laker list | search <q> | show <sha> | get <sha|source:path> [--at] | ls <source>[:dir] [--at]
laker reindex | state rebuild [--force] | snapshot | restore
laker views          # DuckDB SQL over the lake
```

With `common.web.enabled`, serve keeps the state and index open for its whole life; the CLI
commands that need them (`search`, `show`, `ls`, `list`, `run`, `snapshot`) go through serve's
`/api/*` when it answers (`cmd/laker/remote.go`), and open the local files otherwise. The web
pages are `html/template` in `intern/adapters/web/templates`; never mark indexed or mail content
as `template.HTML` without escaping it first (see `markFragment`, `safeHTML`).

The sqldb tests need real servers and are skipped otherwise:
`LAKER_TEST_PG_DSN=postgres://… LAKER_TEST_MYSQL_DSN='user:pw@tcp(host:3306)/' make test`.

A new source implements `out.Source`: read the checkpoint with `rs.GetState`, put each content
with `rs.PutBlob`, append rows with `parquetx.Append(rs, prefix, row)`, index with
`rs.IndexMail/IndexFile`, stage the new checkpoint with `rs.SetState`, and `rs.Commit` every
`commitevery` items. Register it in `cmd/laker/builders.go`.

## Configuration

`/etc/laker/config.yaml` or `./config.yaml` (`--config` overrides). Decoding is strict
(`UnmarshalExact`): an unknown key is an error. `${VAR}` in any value, adapter params included,
is read from the environment and must be set. `config.Validate` reports every problem at once.
Bump `config.SchemaVersion` whenever a yaml key is added, renamed, removed or changes type.

## Log and error codes

Every log line and every `fmt.Errorf`/`errors.New` carries a unique code whose
prefix matches the level (`ERR` `WRN` `INF` `DBG` `FTL`; error values are always `ERR`), three hex
digits, from the package's block:

| Block | Package |
|---|---|
| `0xx` | `cmd/laker` |
| `1xx` | `intern/core` |
| `2xx` | `intern/adapters/sources/*` |
| `3xx` | `intern/adapters/lake/*` |
| `4xx` | `intern/adapters/web` |
| `5xx` | indexer, state, extract adapters |
| `6xx` | `intern/adapters/agentapi`, `agentclient` |
| `8xx` | `cmd/laker-agent` |
| `7xx` | `pkg/*` |

Next free code: `grep -rhoE "ERR3[0-9a-fA-F]{2}" intern/adapters/lake/ | sort -u | tail -1`.
`TestErrorCodesAreUnique` and `TestEveryLogLineHasACode` (`cmd/laker/main_test.go`) enforce it.

Component loggers come from one `logger.Factory` (one Betterstack writer per process, closed on
shutdown), each with a colored prefix: `[LAKER]` white, `[SCHED]` yellow, `[SRC:<name>]` cyan,
`[LAKE]` green, `[INDEX]` magenta, `[WEB]` blue, infrastructure grey. Never log mail or file
contents, nor credentials.

## Git

Develop on the designated branch; "release" means stage, commit, merge into `master`, delete the
branch, tag `v0.<minor>.<patch>`, push branch and tag. Confirm the version before tagging.
