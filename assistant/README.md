# Operator assistant

The Python runtime installs an isolated OpenCode assistant over dwt. Use Python
3.13, `uv`, and the supported OpenCode binary on macOS. Build dwt with `make build`
and prepare the repository's `config.json`, `.env`, and `taxonomy.json` first.

Install **dev first**, supplying your own recipient and an Azure key through the
installing process's environment:

```sh
export AZURE_API_KEY='…'
make assistant-install PROFILE=dev DEV_RECIPIENT=dev@example.com
```

Once verified, install the operator profile:

```sh
make assistant-install PROFILE=operator
```

For the operator, install defaults `recap_recipient` to the live config's sole
`digest_recipients` entry. If there are multiple entries, choose explicitly:

```sh
uv run --project assistant python -m assistant install --repo "$PWD" \
  --profile operator --recap-recipient operator@example.com
```

The chosen address must be in the live config's `digest_recipients` (case
insensitively). The assistant recap goes only to `recap_recipient`; other digest
recipients do not receive it. Dev always uses `--dev-recipient`. Recap checks the
current config before exporting conversations or requesting a paid summary.

Install reads only the `azure-ai` provider from
`~/.config/opencode/opencode.jsonc`, retaining the configured model. To use an
explicit provider JSON/JSONC file (either the provider block or a config containing
`providers.azure-ai`), or to create a Desktop launcher:

```sh
uv run --project assistant python -m assistant install --repo "$PWD" \
  --profile operator --provider-json /path/to/provider.json --desktop-launcher
```

Literal credentials in provider fields are refused; use `{env:AZURE_API_KEY}`.
The key is saved atomically in `secrets.env` with mode 0600. Reinstall refreshes
generated files and the dev database copy, preserves notes and settings, and
reuses the stored provider unless `--provider-json` supplies a replacement.

Each profile lives at `<state>/assistant/<profile>/` (mode 0700). `<state>` is
dwt's state directory, normally `~/Library/Application Support/dontwastetime`.
Install resolves the live directory in this order: process `DWT_STATE_DIR`,
`DWT_STATE_DIR` in the repository's `.env`, config.json `state_dir`, then the
macOS default. Only that key is read from `.env` during resolution.
`--state-root PATH` overrides where assistant profiles are stored; install still
uses the resolved live directory for Operator and as the source of the dev DB
copy. The profile contains:

- `venv/`, `settings.json`, `secrets.env`, and `Assistente.command`.
- `harness-live/`, the generated `workspace/`, and persistent `memory/`.
- Isolated OpenCode homes/stores, plus `logs/` and later exports/snapshots/bench data.
- For dev, `dwt/{dwt.db,config.json,taxonomy.json}`. Only the developer receives
  mail from this config. Both profiles use the repository's `.env` for Graph.

Double-click `Assistente.command` to open a private loopback web UI. Keep its
Terminal window open; closing it stops the server. Pairing credentials exist only
in memory. `python -m assistant launch --profile dev --state-root PATH --tui`
provides a terminal fallback. Starting a new UI re-renders `harness-live`;
launching again reopens the running UI without replacing its workspace.
`ui.json` records its PID and loopback port next to `ui.pid` while it runs.

`settings.json` includes `model`, `improve_every_hours` (24), `soft_cap_usd` (100),
and `model_cost` (null, or `input`, `output`, `cache_read` USD per million tokens).
It also records absolute `opencode_bin`, `dwt_bin`, `dwt_config`, `dwt_env`, and
`secrets_env` paths and the secret-free `provider` block.
`recap_recipient` records the one assistant recap destination. `dwt_state_dir`
records the resolved live state directory for Operator or `<profile>/dwt` for dev,
so later environment changes do not redirect an installed profile's database.
Older settings files remain supported: an unset recipient defaults to the sole
current digest recipient, and an unset state directory is resolved at runtime
(dev retains its private directory).

The runtime depends on OpenCode 2.2.7 behavior. Install and launch print a warning
to stderr if `opencode --version` differs or cannot be checked; startup continues.

The profile's launchd job is
`~/Library/LaunchAgents/com.local.dontwastetime.assistant.<profile>.plist`, logging
to `logs/tick.log` at login, every 15 minutes, and 09:00.
Use `install --no-launchd` for a local trial
without registering a new job; an already-registered job is kept.

```sh
make assistant-uninstall PROFILE=dev          # keep profile state
make assistant-uninstall PROFILE=dev PURGE=1  # delete profile state too
```

Uninstall removes the assistant job and its launcher (including an identical
Desktop copy). dwt's job and live database/config remain in place.

To move to the operator's Mac, copy the repository, configure dwt there, build it,
then run install there with `AZURE_API_KEY` in the environment and
`--provider-json` pointing at a secret-free provider file. Install generates that
Mac's paths and venv; do not transplant the development venv or sessions.

Promotion workflow: to be documented in Task 17.
