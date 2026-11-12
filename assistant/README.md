# Operator's assistant

The Python runtime installs an isolated OpenCode assistant over dwt. Use Python
3.13, `uv`, and the supported OpenCode binary on macOS. Build dwt with `make build`
and prepare the repository's `config.json`, `.env`, and `taxonomy.json` first.

Install **dev first**, supplying your own recipient and an Azure key through the
installing process's environment:

```sh
export AZURE_API_KEY='…'
make assistant-install PROFILE=dev DEV_RECIPIENT=dev@example.com
```

Once verified, install Operator's profile:

```sh
make assistant-install PROFILE=operator
```

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
`--state-root PATH` overrides this location. The profile contains:

- `venv/`, `settings.json`, `secrets.env`, and `Assistente.command`.
- `harness-live/`, the generated `workspace/`, and persistent `memory/`.
- Isolated OpenCode homes/stores, plus `logs/` and later exports/snapshots/bench data.
- For dev, `dwt/{dwt.db,config.json,taxonomy.json}`. Only the developer receives
  mail from this config. Both profiles use the repository's `.env` for Graph.

Double-click `Assistente.command` to open a private loopback web UI. Keep its
Terminal window open; closing it stops the server. Pairing credentials exist only
in memory. `python -m assistant launch --profile dev --state-root PATH --tui`
provides a terminal fallback. Every launch re-renders `harness-live`.

`settings.json` includes `model`, `improve_every_hours` (24), `soft_cap_usd` (100),
and `model_cost` (null, or `input`, `output`, `cache_read` USD per million tokens).
It also records absolute `opencode_bin`, `dwt_bin`, `dwt_config`, `dwt_env`, and
`secrets_env` paths and the secret-free `provider` block.

The profile's launchd job is
`~/Library/LaunchAgents/com.local.dontwastetime.assistant.<profile>.plist`, logging
to `logs/tick.log` at login, every 15 minutes, and 09:00. Until Task 11, tick exits
with “tick non ancora disponibile”. Use `install --no-launchd` for a local trial
without registering the job.

```sh
make assistant-uninstall PROFILE=dev          # keep profile state
make assistant-uninstall PROFILE=dev PURGE=1  # delete profile state too
```

Uninstall removes the assistant job and its launcher (including an identical
Desktop copy). dwt's job and live database/config remain in place.

To move to Operator's Mac, copy the repository, configure dwt there, build it,
then run install there with `AZURE_API_KEY` in the environment and
`--provider-json` pointing at a secret-free provider file. Install generates that
Mac's paths and venv; do not transplant the development venv or sessions.

Promotion workflow: to be documented in Task 17.
