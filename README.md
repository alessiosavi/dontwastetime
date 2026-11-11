# DontWasteTime (`dwt`)

Local email triage for the Example Microsoft 365 mailboxes. `dwt` reads every
enabled mailbox via Microsoft Graph, classifies each mail into the cassetti
taxonomy (`taxonomy.json`), tracks threads that need a reply, and emails a
digest every working day at 08:00 Europe/Rome plus an immediate alert for P0
client threads.

**Read-only on the mailboxes.** v1 never moves, labels, marks or deletes mail.
The only mail it sends is the digest and the P0 alerts, from `digest_sender`
(default `admin@example.com`, `saveToSentItems:false`).

## How it works

```
pollers (one per mailbox, every 3 min, inbox + sentitems, 30-min overlap)
   │ insert message + classify job in one transaction (dedupe: mailbox + internetMessageId)
   ▼
SQLite queue (WAL) ──► worker pool (4)
                         classify       rules first (headers, attachments, senders) → LLM for the rest
                         thread_refresh needs-reply state, Clienti priority, P0 alert (once per thread per day)
                         digest         working days 08:00 Europe/Rome
scheduler: digest · SLA sweep every 15 min · mailbox verification hourly · body retention daily
```

State lives in `~/Library/Application Support/dontwastetime/` (override with
`state_dir`, `DWT_STATE_DIR` or `--state-dir`): `dwt.db`, `digests/`,
`reports/`, `alerts/` (dry runs) and `logs/`.

## Setup checklist

1. **Credentials**: `cp .env.example .env` and copy `MS_TENANT_ID`,
   `MS_CLIENT_ID` and `MS_CLIENT_SECRET` by hand from internal's local `.env`. Never
   commit `.env` (it is gitignored) and never paste its values anywhere else.
2. **Classifier**: set `LLM_URL`, `LLM_MODEL` and `LLM_API_KEY` for the Azure AI
   Foundry deployment (EU hosting). `LLM_URL` is the OpenAI-compatible base:
   `dwt` POSTs to `${LLM_URL}/chat/completions`.
3. **Config**: `cp config.example.json config.json`, then set:
   - `mailboxes`: every mailbox to follow. `digest:false` keeps a box classified
     for coverage but out of the digest and alerts (employee personal boxes).
   - `digest_recipients`: the founder's address and Operator's chosen address.
     `alert_recipients` defaults to the same list.
   - `top_clients`: the 10–20 client domains that can trigger P0 alerts.
4. `make check build`.

## First run (in this order)

```bash
./bin/dwt doctor                                   # everything green?
./bin/dwt mailboxes verify                         # every mailbox readable?
./bin/dwt mailboxes discover                       # list readable tenant mailboxes (no changes)
./bin/dwt sync --mailbox marketing@example.com       # first small real sync
./bin/dwt explore --days 90 --mailboxes marketing@example.com,tech@example.com,business@example.com
#   read the reports in <state>/reports/, then admin@ and the personal boxes;
#   Operator + founder sign the cluster table (approve / rename / reject)
./bin/dwt digest --date today --dry-run            # review <state>/digests/<day>.md
./bin/dwt serve --dry-run                          # full loop, nothing sent
make install                                       # launchd agent (KeepAlive)
```

The classifier is trusted on the main accounts only after the phase-0
sign-off. Re-run `explore` monthly for the first three months, or whenever
`doctor` reports an `unsure` share above 15%.

## Commands

| Command | What it does |
|---|---|
| `dwt serve [--dry-run]` | Everything: pollers, workers, scheduler. `--dry-run` saves digests and alerts under the state dir and sends nothing. |
| `dwt sync [--mailbox a] [--initial-days n]` | One sync pass (first sync reads `initial_days`, default 90). |
| `dwt classify [--limit n]` | Drain pending classify and thread jobs once (no alerts). |
| `dwt digest [--date YYYY-MM-DD\|today] [--dry-run]` | Build the digest for that day's 08:00 and send it, at most once per day. |
| `dwt explore [--days n] [--mailboxes a,b] [--max-messages n]` | Phase-0 clustering reports. |
| `dwt mailboxes verify` / `discover [--write-config]` | Probe access; list the tenant's readable mailboxes (member accounts whose inbox the app can read; an alias of a configured box is not listed twice) and merge new ones into `config.json` (`digest:false`, backup in `.bak`). |
| `dwt correct <id> <label>` | Relabel a message. Ids are the `#N` at the end of digest lines; corrections become few-shot examples. |
| `dwt mute-thread <id>` | Silence a thread resolved off-mail (message `#N` or thread key). |
| `dwt doctor` | Config, credential, per-mailbox access, queue, dead-letters, budget, unsure share, disk. Exits non-zero on any red item. |

Every command accepts `--config`, `--state-dir`, `--env` and `--taxonomy`
(`.env` and `taxonomy.json` default to the config file's directory).

## Operations

- **launchd**: `make install` runs `doctor`, renders
  `deploy/com.local.dontwastetime.plist` into `~/Library/LaunchAgents`, and
  bootstraps it. Run it from the checkout that holds `config.json` and `.env`.
  `make uninstall-agent` removes it.
- **Logs**: `<state>/logs/dwt.err.log` (JSON). Logs carry ids and counts, never
  bodies, tokens or secrets.
- **Fail-closed**: missing config or credentials refuse to start; if no mailbox
  is readable at boot, `serve` exits. A single unreadable mailbox (403/429) is
  marked degraded with a reason and shown in the digest; the others keep working.
- **Budget**: `LLM_DAILY_MAX_TOKENS` (default 2,000,000) covers every LLM stage.
  When it is reached, classification pauses (jobs wait, nothing is lost) and the
  digest says so.
- **Restarts**: ingest and enqueue are one transaction; a restart resets
  interrupted jobs, clears stale leases and re-enqueues every unclassified
  message. Digests missed while the Mac slept go out when it wakes (the 3 most
  recent working days at most), each at most once per day. A digest whose
  send keeps failing is retried every 15 minutes for the rest of the day; a
  digest or P0 alert whose send got no answer from Graph is not retried, since
  it may already have been delivered.
- **Retention**: message bodies older than `retention_days` (365) are purged
  daily; metadata stays.

## Privacy

Bodies are stored locally in SQLite and sent only to the configured LLM endpoint
(EU hosting), as an excerpt of at most 1,000 characters. Digests and alerts go
only to the configured recipients. Credentials are env-only and never logged.

## Development

`make check` = `gofmt -l` + `go vet ./...` + `go test -race ./...`. Tests use
an in-process fake Graph (`internal/graph/graphtest`) and a fake LLM
(`internal/classify/llmtest`); `internal/pipeline` has a golden end-to-end test
on 50 synthetic messages (`go test ./internal/pipeline -update` rewrites the
golden; review the diff). Only `cmd/dwt` reads the wall clock (enforced by a
test).
