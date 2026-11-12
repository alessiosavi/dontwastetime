# OpenCode 2.2.7 facts — Task 1 spike

Measured on this Mac on 2026-11-12. Analysis only: no repository edits, Git operations,
mail data, developer service connections, or developer credential/config reads.
`/opt/homebrew/bin/opencode --version` returned `opencode v2.2.7` (exit 0).
All model invocations used `azure-ai/DeepSeek-V4.1-Flash-1017`, optionally with a
variant suffix. Nine short user prompts were sent across the experiments; no more
model runs are needed to reproduce the structural checks.

The four deliverables are self-contained evidence; raw captures and probe scripts
remain at `/tmp/dwt-dev/task1/scratch/`. Outputs below are trimmed,
not synthetic. `VERIFIED` marks the measured answer, including negative answers;
it does not imply every future integration or security property was tested.
Claude's independent reruns of Q1, Q3 and Q5 are recorded in the final section, "Claude verification", which also corrects the isolation recipe (do not set `OPENCODE_DISABLE_PROJECT_CONFIG`).

## 1. Isolation — VERIFIED

Use an allowlisted child environment, an isolated HOME, all four XDG homes,
an isolated config directory, one explicit config file, and `--standalone` for
commands that connect to a server. Disable project discovery and Claude discovery.
Render **absolute** `instructions` paths with this recipe (Q2).

The final tested environment, with `R=/tmp/dwt-dev/task1/scratch/final`,
is exactly the following. The probe preserves PATH and optional locale/certificate
variables by name; it never prints credential values. It passes AZURE_API_KEY
through the child environment and has no inherited OPENCODE_* variables.

```python
# Equivalent to scratch/probe.py:environment(); run with cwd=R / "workspace".
e = {k: os.environ[k] for k in (
    "PATH", "TMPDIR", "LANG", "LC_ALL", "AZURE_API_KEY",
    "SSL_CERT_FILE", "SSL_CERT_DIR"
) if k in os.environ}
e.update(
    HOME=str(R / "home"),
    XDG_CONFIG_HOME=str(R / "config"),
    XDG_DATA_HOME=str(R / "data"),
    XDG_CACHE_HOME=str(R / "cache"),
    XDG_STATE_HOME=str(R / "state"),
    TMPDIR=str(R / "tmp"),
    OPENCODE_CONFIG_DIR=str(R / "opencode-config"),
    OPENCODE_CONFIG=str(R / "workspace/opencode.json"),
    OPENCODE_DISABLE_PROJECT_CONFIG="true",
    OPENCODE_DISABLE_CLAUDE_CODE="true",
    NO_COLOR="1",
)
# Create the directories first. Supply the key from secrets_env as $AZURE_API_KEY.
# subprocess.run(["/opt/homebrew/bin/opencode", ...], cwd=R/"workspace",
#                env=e, stdin=subprocess.DEVNULL, timeout=120, ...)
```

This is an observed working recipe, not a claim that every variable is individually
necessary. HOME also isolates non-XDG discovery; XDG_CONFIG_HOME alone is not the
recipe. Do not inherit `OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG`, `--server`,
or other developer overrides. Set HOME only for the child process.
`OPENCODE_CONFIG_DIR` wins for the reported config path; the explicit workspace
config is loaded even when project discovery is disabled. Do not put a second
copy in that config directory: earlier probes did and saw it twice in the sources.

For compact commands below, `oc ...` denotes the exact binary argv under this
child environment and cwd. Actual recorded commands follow.

```sh
/opt/homebrew/bin/opencode session list --standalone
/opt/homebrew/bin/opencode debug paths
/opt/homebrew/bin/opencode debug config --standalone
/opt/homebrew/bin/opencode debug agents --standalone
/opt/homebrew/bin/opencode api GET /api/plugin --standalone
/opt/homebrew/bin/opencode api GET /api/skill --standalone
python3 /tmp/dwt-dev/task1/scratch/audit.py
```
All exited 0. `session list` was run before any model run in the fresh `final`
home; stdout and stderr were both empty. After the fixture runs, sessions existed
only in that isolated data home. `debug paths` is a local command and does not
accept `--standalone`; it does not connect to a server.

Observed paths:

```text
home       /tmp/dwt-dev/task1/scratch/final/home
data       /tmp/dwt-dev/task1/scratch/final/data/opencode
cache      /tmp/dwt-dev/task1/scratch/final/cache/opencode
config     /tmp/dwt-dev/task1/scratch/final/opencode-config
state      /tmp/dwt-dev/task1/scratch/final/state/opencode
tmp        /tmp/dwt-dev/task1/scratch/final/tmp/opencode
bin        /tmp/dwt-dev/task1/scratch/final/cache/opencode/bin
log        /tmp/dwt-dev/task1/scratch/final/data/opencode/log
repos      /tmp/dwt-dev/task1/scratch/final/data/opencode/repos
db         /tmp/dwt-dev/task1/scratch/final/data/opencode/opencode.db
```
Observed config/discovery and persisted instruction audit (trimmed):

```json
{
  "config_sources": [
    [
      "directory",
      "/tmp/dwt-dev/task1/scratch/final/opencode-config"
    ],
    [
      "document",
      "/tmp/dwt-dev/task1/scratch/final/workspace/opencode.json"
    ]
  ],
  "agent_ids": [
    "build",
    "compaction",
    "explore",
    "general",
    "plan",
    "summary",
    "title"
  ],
  "plugins": [],
  "skills_from_api": [],
  "core_instructions": [
    {
      "path": "/tmp/dwt-dev/task1/scratch/final/workspace/memory/notes.md",
      "content": "Always-loaded synthetic marker: NOTES_MARKER_7b6a42.\n"
    }
  ],
  "forbidden_markers_found": [],
  "credential_count": 0,
  "auth_json_count": 0,
  "write_probe_file_exists": false
}
```
`debug config` in 2.2.7 returns ordered **configuration sources**, not one merged
config object. Effective agents were checked separately. To prove the actual
always-loaded file set, `audit.py` opened **only the isolated OpenCode DB** using
SQLite URI `mode=ro`, looked up the absolute-marker session in `instruction_state`,
and dereferenced its `core/instructions` hash in `instruction_blob`. That snapshot
contains only the notes file above. It has no developer global AGENTS.md text,
runner/e2e-tester definitions, claude-mem plugin, or developer home paths. The
built-in OpenCode/report skill guidance is distinct from developer-installed skills.

Conclusion: the recipe isolates the tested config, agents, plugins, instructions,
credentials and session store, and all connecting commands use a private server.
For a persistent UI, launch your own `serve` process in this same environment
(Q9); never use `serve --service`. Local-only commands such as `debug paths` and
help do not take `--standalone`.

Plan correction (Task 4/7): isolate HOME and TMPDIR too; use a child environment
allowlist; disable project discovery and render absolute instruction paths.
Keep profile config isolated and shared between ui/ops, but select **different
XDG_DATA_HOME values** for the two stores. Preserve Harness²'s per-session data
and state overrides for its improver. No ui/ops application wrapper was implemented
or tested here; fresh independent homes demonstrate the underlying store behavior.

## 2. Always-on files — VERIFIED

With project discovery enabled, workspace `opencode.json` containing
`{"instructions":["memory/notes.md"]}` loaded the relative file. Its contents were
`Always-loaded synthetic marker: NOTES_MARKER_7b6a42.` The marker value was absent
from the user prompt, and no read call was made.

The first experiment used R=`scratch/isolated`, OPENCODE_CONFIG_DIR=
`R/opencode-config`, OPENCODE_CONFIG=`R/opencode-config/opencode.json`, project
config disabled=`false`, and the otherwise isolated environment from Q1. The
initial run also forwarded AZURE_ENDPOINT; later runs did not need it.

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 'This is a synthetic fixture. State the always-loaded marker from memory/notes.md without reading a file. Then call notes echo_note exactly once with text synthetic-task1. Finally say DONE. No other tools.'
```
Observed (exit 0): `text: NOTES_MARKER_7b6a42`, one `notes_echo_note` call, then `DONE`.

There is an important second result. In the strict Q1 environment, the same
relative instructions entry still appeared in `debug config`, but the model did
**not** receive it:

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 'This is a synthetic fixture. State the always-loaded marker from memory/notes.md without reading a file. Then call notes echo_note exactly once with text synthetic-task1. Finally say DONE. No other tools.'
```
Observed text (exit 0):

```text
I don't have the contents of `memory/notes.md` loaded in my context, so I can't state its always-loaded marker from memory.

DONE
```
Root cause was read from the installed binary's bundled
`ConfigInstructionPlugin.match`: when project discovery is enabled it uses
`globUp(pattern, workspace, project)`; when disabled, relative patterns are scanned
with `cwd: isolated_config_directory`. Absolute paths are scanned directly in both
modes. Merely seeing the entry in config is insufficient proof of loading.

Changed only `instructions` to the absolute file path:

```json
{
  "instructions": [
    "/tmp/dwt-dev/task1/scratch/final/workspace/memory/notes.md"
  ]
}
```
```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model 'azure-ai/DeepSeek-V4.1-Flash-1017#max' 'Return the always-loaded marker from memory/notes.md without calling any tool, or MISSING if absent. Nothing else.'
```
Observed (exit 0): `NOTES_MARKER_7b6a42`, zero tools. The persisted `core/instructions` snapshot in Q1 confirms that exact file and text.

Conclusion: relative workspace instructions work with project discovery;
absolute instructions work with the stricter isolation recipe. The saved fixture
intentionally retains the relative-path negative case, before the correction.

Plan correction (Task 7): render absolute paths for the harness, notes, recap and
status files when using Q1. Apply the existing size limits to their contents;
this spike did not test live reload, missing-file handling or the full 24,000-character
bundle. No additional plan contradiction.

## 3. Permissions — VERIFIED

Canonical 2.2.7 schema: top-level `permissions`, an ordered array of
`{action, resource, effect}`. The exact tested restrictive policy is:

```json
{
  "permissions": [
    {
      "action": "*",
      "resource": "*",
      "effect": "deny"
    },
    {
      "action": "read",
      "resource": "*",
      "effect": "allow"
    },
    {
      "action": "skill",
      "resource": "*",
      "effect": "allow"
    },
    {
      "action": "notes_*",
      "resource": "*",
      "effect": "allow"
    }
  ]
}
```
The first rule denies every action, including bash, shell, edit, write, patch,
webfetch, websearch, task, subagent, execute, external_directory and future actions.
Later rules allow read, skill and the explicitly named stub MCP namespace. For
production, add an equivalent `dwt_*` allow rule only when installing dwt; no dwt
server was configured or tested in this spike. Do not replace the deny-all rule
with a finite deny list. Built-in agent rules appear before the config rules;
`debug agents` showed the restrictive policy after them. The browser skill has an
additional built-in deny.

In 2.2.7 the actual shell permission is `shell`, subagents use `subagent`, and
write/patch tools authorize against `edit`. The wildcard denial covers the older
bash/task names as well. Read paths inside this non-Git workspace are relative;
reads outside it require `external_directory`, which stays denied.

Exact acceptance commands, all exit 0:

```sh
/opt/homebrew/bin/opencode debug agents --standalone
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 --session ses_e32d1389bffeWkff0K3fth8DPG 'Create permission-proof.txt in the current workspace containing CREATED. Try a real filesystem write using edit, write, patch, or shell/bash. Do not use the notes echo tool. If writing is impossible, reply SECOND_TURN_BLOCKED. Be brief.'
python3 /tmp/dwt-dev/task1/scratch/audit.py
```
Observed: `SECOND_TURN_BLOCKED`; zero tool calls on that turn;
`permission-proof.txt` absent before and after. This is a denial result despite
`run` exiting 0, because the model completed its response. The write tools were
unavailable; this probe did not manufacture a denied write event.

A positive and negative read check used the discovery-enabled isolated environment
from Q2, with the **same** permissions JSON and non-Git workspace:

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 'Use the read tool to read memory/notes.md. Then use the read tool to try ../outside-proof.txt; this is synthetic test data only. Report allowed or denied for each. Do not use other tools. Be brief.'
```
```text
read {"path": "memory/notes.md"}: completed 
read {"path": "../outside-proof.txt"}: error Permission denied: external_directory
```
`../outside-proof.txt` held only a synthetic marker. No secret paths were probed.
MCP success is demonstrated in Q4. Skill is allowed in the effective rules; a skill
tool invocation was not separately exercised. This is OpenCode tool authorization,
not an OS sandbox; workspace-only behavior in other Git/project layouts is unverified.

Harness² compatibility check (no model call):

```python
# scratch/probe.py run(..., extra_env=...)
extra_env = {"OPENCODE_PERMISSION": '{"read":"deny"}'}
# argv: /opt/homebrew/bin/opencode debug agents --standalone
```

Observed exit 0: the effective build rules still allow read, with no appended
read-deny rule. `OPENCODE_PERMISSION` is absent from the installed binary's bundled
source; the behavioral result confirms this old variable did not apply here.

Plan correction: Task 7 must render the ordered policy above. Task 4/improver shim
must translate Harness²'s OPENCODE_PERMISSION restriction into 2.2.7 configuration,
for example a generated `OPENCODE_CONFIG_CONTENT` rules array; do not rely on that
old env var to confine edits. Verify the adapter's edit-boundary enforcement before
running an improver. Design §5.2's direct notes-file writes conflict with the plan's
live deny-all-writes policy; follow the plan and use the notes MCP path.

## 4. MCP naming — VERIFIED

Exact installed MCP configuration used (no mail or network behavior):

```json
{
  "mcp": {
    "servers": {
      "notes": {
        "type": "local",
        "command": [
          "/opt/homebrew/bin/python3",
          "/tmp/dwt-dev/task1/scratch/echo_mcp.py"
        ],
        "codemode": false,
        "protocol": "legacy"
      }
    }
  }
}
```
The stdio stub exposes one tool, `echo_note(text)`, and returns `NOTE:` plus the
input. Its entire source is included in the fixture README. Explicit
`codemode:false` exposes a direct tool; the schema says Code Mode defaults to true.

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 'This is a synthetic fixture. State the always-loaded marker from memory/notes.md without reading a file. Then call notes echo_note exactly once with text synthetic-task1. Finally say DONE. No other tools.'
/opt/homebrew/bin/opencode session export ses_e32d1389bffeWkff0K3fth8DPG --standalone
```
Observed event (trimmed):

```json
{
  "type": "tool_use",
  "part": {
    "tool": "notes_echo_note",
    "state": {
      "status": "completed",
      "input": {
        "text": "synthetic-task1"
      },
      "output": "NOTE:synthetic-task1",
      "title": "notes_echo_note",
      "metadata": {
        "metadata": {
          "truncated": false
        },
        "content": [
          {
            "type": "text",
            "text": "NOTE:synthetic-task1"
          }
        ],
        "providerCall": {
          "executed": false
        },
        "providerResult": {
          "executed": false
        },
        "rawInput": "{\"text\": \"synthetic-task1\"}"
      },
      "time": {
        "start": 1794443761027,
        "end": 1794443761060
      }
    }
  }
}
```
Observed export tool (trimmed):

```json
{
  "type": "tool",
  "id": "call_0222d1d3506d4ee6ab3055f0",
  "name": "notes_echo_note",
  "executed": false,
  "state": {
    "status": "completed",
    "input": {
      "text": "synthetic-task1"
    },
    "content": [
      {
        "type": "text",
        "text": "NOTE:synthetic-task1"
      }
    ],
    "metadata": {
      "truncated": false
    }
  },
  "time": {
    "created": 1794443761027,
    "ran": 1794443761045,
    "completed": 1794443761060
  }
}
```
Conclusion: direct MCP names are `<server>_<tool>`, here `notes_echo_note`.
Events use `part.tool`; exports use `messages[].content[].name`. Completed export
output is `state.content[]`, whereas the run event includes flattened
`state.output`. `executed:false` is present on this successfully completed local
call; do not interpret that field as execution failure. Use state.status and the
returned content.

Plan correction (Task 7/tool guidance/export parsing): use `mcp.servers`, set
`codemode:false` for direct named tools, and allow their server prefixes. Do not
assume the older `mcp.<server>.enabled` shape or Code Mode's event representation.
Only this simple server/tool naming case was measured.

## 5. Run events and Harness² — VERIFIED

Exact capture command is Q4's `canonical-one` command. The saved JSONL contains
six event lines and exactly one tool call:

```text
step_start / part.type=step-start
tool_use / part.type=tool
step_finish / part.type=step-finish
step_start / part.type=step-start
text / part.type=text
step_finish / part.type=step-finish
```
Each event has `type`, epoch-millisecond `timestamp`, `sessionID`, and `part`.
`text` lives in `part.text`. Tool calls use `tool_use`/`part.type:tool` with a
completed/error state. `step_finish` uses `part.type:"step-finish"`, `part.cost`,
`part.tokens` and `part.reason`. The events retain the legacy CLI projection even
though the native v2 event API has different event names. Do not substitute SSE
`/api/event` data for this fixture.

Imported the **actual, unmodified clone** through PYTHONPATH and ran both helpers:

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=/path/to/harness2 python3 /tmp/dwt-dev/task1/scratch/harness_parse.py
```
Observed (exit 0):

```json
{
  "import_from": "/path/to/harness2/harness2/core/proposer.py",
  "text": "I don't have the contents of `memory/notes.md` loaded in my context, so I can't state its always-loaded marker from memory.\n\nDONE",
  "error": "",
  "cost": 0.0,
  "ROOT": "/path/to/harness2",
  "BUN": "/path/to/harness2/dependencies/tools/node_modules/.bin/bun",
  "BUN_exists": false,
  "blocked_env_reads": [
    "/path/to/harness2/.env"
  ]
}
```
The parser text matches all captured text events, error is empty, and cost is
0.0, matching the captured step costs. A Python audit hook rejected `.env` opens
before importing Harness²; `load_dotenv` catches that PermissionError. No .env was
read, no Harness² module was monkeypatched, and bytecode writes were disabled.
The hook source is included in the fixture README.

Variant commands (earlier isolated discovery-enabled environment from Q2):

```sh
/opt/homebrew/bin/opencode run --standalone --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 --variant high 'Reply OK only.'
/opt/homebrew/bin/opencode run --standalone --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 --variant max 'Reply OK only.'
/opt/homebrew/bin/opencode run --standalone --auto --format json --model 'azure-ai/DeepSeek-V4.1-Flash-1017#high' 'Reply HIGH_OK only. No tools.'
/opt/homebrew/bin/opencode run --standalone --auto --format json --model 'azure-ai/DeepSeek-V4.1-Flash-1017#max' 'Reply MAX_ENV_ONLY_OK only. No tools.'
```
Observed: both `--variant` commands exited **1** with
`Unrecognized flag: --variant in command opencode run`. Both suffix commands exited
**0**, returning HIGH_OK / MAX_ENV_ONLY_OK. Their exports recorded respectively
`info.model.variant: high` and `max`. The final Q1 recipe also ran `#max` successfully
in the absolute-marker check (Q2).

Acceptance of `#high` does **not** prove high reasoning effort reached Azure: the
provided model has only an explicit max variant, with max settings/body defaults.
Wire-level effective effort was not measured. If high effort matters, render and
validate an explicit high variant before relying on the suffix.

Read-only Harness² findings (`proposer.py:244-303,389-400,439-499`, `config.py`):

- `_opencode` writes MISSION.md and a legacy `agent.build.steps` workspace config;
  sets OPENCODE_PERMISSION to an external-directory allowlist; overrides
  XDG_DATA_HOME=`cwd/.ocdata` and XDG_STATE_HOME=`cwd/.ocstate`.
- It invokes `config.BUN run --cwd <ROOT/dependencies/opencode/packages/opencode>
  --conditions=browser src/index.ts run <prompt> --dir <cwd> --model <model>
  --format json`, then appends `--variant <effort>` when configured. It does not add
  `--standalone`. This path targets its own source checkout, not the installed CLI.
- The installed CLI also rejects `--dir` (exit 1). Use subprocess cwd instead.
- The old config input `{"agent":{"build":{"steps":3}}}` was accepted through
  OPENCODE_CONFIG_CONTENT and normalized to `agents.build.steps` (debug exit 0);
  do not call that singular key an established incompatibility. `agents` is canonical.
- Import-time ROOT is the package parent **when that parent has pyproject.toml**,
  otherwise cwd. BUN is always ROOT/dependencies/tools/node_modules/.bin/bun.
  In this source clone, changing cwd before import does not relocate ROOT; BUN is
  absent. RUNS and model settings are also fixed at first import.

Plan correction: retain the two compatible parser helpers. The planned shim must
translate `--dir`, `--variant`, permission env and standalone behavior, preserve
per-session stores, and receive the improver-specific provider-only config. Setting
cwd before import alone does not redirect this source clone's ROOT. Verify the
actual installed Harness² ROOT/BUN before placing shim files. No shim, proposer run
or write-capable improver was implemented/tested in this spike.

## 6. Session export — VERIFIED

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 --session ses_e32d1389bffeWkff0K3fth8DPG 'Create permission-proof.txt in the current workspace containing CREATED. Try a real filesystem write using edit, write, patch, or shell/bash. Do not use the notes echo tool. If writing is impossible, reply SECOND_TURN_BLOCKED. Be brief.'
/opt/homebrew/bin/opencode session export ses_e32d1389bffeWkff0K3fth8DPG --standalone
```
Both exited 0. The export is a single JSON object with `info` and `messages`,
not the older `messages[].info` / `messages[].parts` representation.

Observed fixture summary:

```json
{
  "info": {
    "id": "ses_e32d1389bffeWkff0K3fth8DPG",
    "projectID": "4f40dab30e052b681921187ec86e2ba7665ce881",
    "model": {
      "id": "DeepSeek-V4.1-Flash-1017",
      "providerID": "azure-ai",
      "variant": "default"
    },
    "cost": 0,
    "tokens": {
      "input": 1846,
      "output": 91,
      "reasoning": 403,
      "cache": {
        "read": 3328,
        "write": 0
      }
    },
    "outcome": "succeeded",
    "time": {
      "created": 1794443757550,
      "updated": 1794443776823,
      "busy": 1794443775660,
      "idle": 1794443776844
    },
    "title": "Synthetic memory note echo",
    "location": {
      "directory": "/tmp/dwt-dev/task1/scratch/final/workspace"
    }
  },
  "message_types": [
    "user",
    "assistant",
    "assistant",
    "idle",
    "user",
    "assistant",
    "idle"
  ]
}
```
Mapping for the later exporter/recap/training code:

| Data | Actual field |
| --- | --- |
| Session ID/model/directory | `info.id`, `info.model.{providerID,id,variant}`, `info.location.directory` |
| Session aggregate cost/usage | `info.cost`, `info.tokens.{input,output,reasoning,cache.{read,write}}` |
| Session times | `info.time.{created,updated,busy,idle}` (epoch milliseconds) |
| User turn | `messages[]` with `type:user`, `text`, `files[]`, `time.created` |
| Assistant message | `type:assistant`, `content[]`, `agent`, `model`, `finish`, `rawFinish` |
| Assistant text/reasoning | `content[]` with `type:text` or `type:reasoning`, each with `text` |
| Tool call | `content[]` with `type:tool`, `id`, `name`, `state.input`, `state.status` |
| Completed tool output | `state.content[]`, e.g. `{type:text,text:"NOTE:synthetic-task1"}` |
| Tool times | `content[].time.{created,ran,completed}` (milliseconds) |
| Assistant times | `time.{created,streamed,completed}` |
| Assistant usage/cost | `tokens`, `cost` directly on the assistant message |
| Non-turn markers | `type:idle`, `outcome`, `time.created` |

There are exactly two user messages, three assistant messages, two idle markers,
and one tool call. The second user turn is the file-write denial. Tool input/output
is synthetic. Session aggregate tokens need not equal the visible assistant sum;
do not add aggregates to message totals. Do not train on reasoning or idle markers
as user/assistant prose. Failure/compaction/attachment shapes were not captured.

Plan correction (Task 4 consumers, export/recap/training tasks): use this v2 export
shape, nested tool content, millisecond timestamps, and explicit message types.
`session export --sanitize` exists, but its redaction behavior was not tested;
these fixtures were independently scrubbed and contain no mail.

## 7. Azure key without an auth store — VERIFIED

Added only this authentication setting to the supplied sanitized provider block:

```json
{"providers":{"azure-ai":{"settings":{"apiKey":"{env:AZURE_API_KEY}"}}}}
```

This is a **fragment**, not a replacement for its name/package/baseURL/model data.
The config file contains the literal placeholder, never a key. The child receives
`AZURE_API_KEY=$AZURE_API_KEY`; no other credential variable, auth.json or copied
credential DB is supplied. The fixed baseURL is in the sanitized block, so
AZURE_ENDPOINT is unnecessary and was absent from the final environment.

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model 'azure-ai/DeepSeek-V4.1-Flash-1017#max' 'Return the always-loaded marker from memory/notes.md without calling any tool, or MISSING if absent. Nothing else.'
python3 /tmp/dwt-dev/task1/scratch/audit.py
```
Observed (exit 0): marker response from Azure; `auth_json_count:0` and isolated
SQLite `credential_count:0`. `debug config` masks the resolved apiKey as `***`.
The audit counts credential rows; it never selects credential contents.

Conclusion: explicit `settings.apiKey:"{env:AZURE_API_KEY}"` works with env-only
credentials and the empty isolated credential store. Automatic key discovery from
the **unmodified** custom azure-ai block was not tested; do not rely on it.

Plan correction (Task 4/7): copy the sanitized provider definition and add that
explicit env interpolation. The sanitized source has neither a key setting nor
model costs. The launcher must populate the key from the profile's secrets source,
not silently fall back to a developer key. This spike passed the authorized shell
key; it did not implement or test secrets-file parsing.

## 8. Cost fields — VERIFIED (zero, not non-zero)

Exact sources inspected: the Q4 run and export commands, and the Q7 absolute-marker
run. Structural checks (no model call):

```python
events = [json.loads(line) for line in Path("run_events.jsonl").read_text().splitlines()]
export = json.loads(Path("session_export.json").read_text())
print([e["part"]["cost"] for e in events if e["type"] == "step_finish"])
print(export["info"]["cost"])
print([m["cost"] for m in export["messages"] if m["type"] == "assistant"])
```

```text
[0, 0]
0
[0, 0, 0]
```
Token counts are non-zero (see Q6), while every tested step/message/session cost
is zero. `_opencode_cost` returning 0.0 is correct parsing, not proof of free Azure
inference. The copied custom model definition has **no cost field**.

The installed `/openapi.json` schema supports model `cost` with `input` and `output`
rates (USD per million tokens), optional `cache:{read,write}`, and optional tiers.
No rates were invented or inserted, and non-zero pricing calculations were not
verified.

Plan correction: the requirement to preserve existing cost entries cannot create
pricing entries that are absent. Obtain validated Azure deployment prices and
render them, or represent spend as **unknown** and retain tokens for later costing.
The $100 warning/recap cannot be verified against these zero-cost records yet.
Actual billed dollars and provider usage reconciliation remain NOT VERIFIED.

## 9. GUI client — VERIFIED (private bundled web client)

Commands:

```sh
ls -1 /Applications
```

```sh
/opt/homebrew/bin/opencode --help
/opt/homebrew/bin/opencode serve --help
/opt/homebrew/bin/opencode acp --help
python3 /tmp/dwt-dev/task1/scratch/web_probe.py
```
No OpenCode desktop app appeared in /Applications. The installed CLI lists
`serve` as a v2 API **and web server**; no `web` subcommand is listed. `acp` starts
an Agent Client Protocol server; it is not itself a desktop UI.

`web_probe.py` launches a separate server in the Q1 environment and workspace:

```sh
/opt/homebrew/bin/opencode serve --hostname 127.0.0.1 --port 57816
```
This is a foreground server, **without `--service`**. It generates an ephemeral
password in memory and passes it as `OPENCODE_PASSWORD` (value never printed or
persisted in the report). An authenticated POST to `/api/pair` produces a private
one-time code; Moli follows `/auth/connect/$PAIR_CODE`, which sets the browser
cookie. Password and pairing material are excluded from all deliverables.

Actual browser argv, with the pairing code replaced by its variable name:

```sh
~/.local/bin/moli fetch --wait done --wait-script 'document.querySelector('"'"'button[aria-label="New session"]'"'"') !== null' --eval '(async()=>{ const r=await fetch('"'"'/api/session'"'"'); const j=await r.json(); return {title:document.title,body:document.body.innerText.slice(-5000),apiStatus:r.status,sessionResponseKeys:Object.keys(j),fixturePresent:JSON.stringify(j).includes('"'"'ses_e32d1389bffeWkff0K3fth8DPG'"'"')}; })()' --timeout 20000 'http://127.0.0.1:57816/auth/connect/$PAIR_CODE'
```
Observed (exit 0):

```json
{"title":"OpenCode","body":"Projects\nAdd project\nSettings\nHelp","apiStatus":200,"sessionResponseKeys":["data","cursor"],"fixturePresent":true}
```
HTTP / returned 200 with the bundled OpenCode page and assets. Moli 1.1.21
rendered Home/Search sessions/New session controls and Projects/Settings/Help.
Its authenticated browser fetch of `/api/session` returned 200 and contained the
fixture ID, proving it reached the isolated store. The private process was then
terminated; exit 143 is expected teardown. It did not touch the developer service.

Conclusion/choice: **private bundled web client**, available in the installed
2.2.7 binary. A double-click launcher can start that isolated foreground server,
create a pairing URL and open it in the user's browser. The TUI remains an available
fallback. Creating/sending a turn through the UI, native desktop behavior and the
final double-click launcher were not exercised.

Plan correction: `opencode web` is not the command on this build; use `serve` with
a private environment, explicit loopback port, managed lifetime and browser pairing.
Do not append `--standalone` to `serve` itself; it starts the server directly.
The unauthenticated initial readiness attempt timed out; authentication/pairing is
part of the working recipe, not an optional usability detail.

Failure ledger (all classified; no unexplained pending process):

| Command/probe | Exit/result | Explanation |
| --- | --- | --- |
| `debug paths --standalone` | 1 | Local command rejects this flag; use `debug paths`. |
| First concurrent `debug config --standalone` | 1 | Private server exited before readiness; sequential retry returned 0. Root cause of that startup race was not proven. |
| `run ... --variant high ...` | 1 | Unrecognized flag. |
| `run ... --variant max ...` | 1 | Unrecognized flag. |
| `run ... --dir <workspace> ...` | 1 | Unrecognized flag. |
| Unauthenticated private GUI readiness probe | Script exit 0, result failed | HTTP readiness never succeeded; timeout recorded, then server terminated (143). Script success was not treated as verification. |
| Relative notes with project discovery disabled | CLI exit 0, acceptance failed | File resolved from config directory; absolute-path follow-up passed. |
| Private GUI server termination | 143 | Intentional teardown, not a startup failure. |

Remaining uncertainties: actual high-effort wire parameters; pricing/billing;
full application ui/ops separation; write-capable improver shim; arbitrary MCP
names/Code Mode; UI prompt sending and launcher lifecycle. Claude should independently
rerun Q1, Q3 and Q5 as the plan requires before acceptance. No delegated reviewer or
Claude review is claimed here.

## Claude verification (2026-11-12) — questions 1, 3, 5, plus harness loading

Independent re-run with a fresh probe (`/tmp/dwt-dev/verify/probe.py`,
not committed) in two environments that differ only in
`OPENCODE_DISABLE_PROJECT_CONFIG`. Both use isolated HOME, the four XDG homes,
TMPDIR, `OPENCODE_CONFIG_DIR`, `OPENCODE_CONFIG=<workspace>/opencode.json`,
`OPENCODE_DISABLE_CLAUDE_CODE=true`, `settings.apiKey: "{env:AZURE_API_KEY}"`, the
ordered deny-all + read + skill `permissions`, and an absolute `instructions` entry.
The workspace holds `AGENTS.md` (marker B) and `.opencode/skills/sonda/SKILL.md`
(marker C); the notes file holds marker A.

| Check | strict (`DISABLE_PROJECT_CONFIG=true`) | discovery (unset) |
|---|---|---|
| `session list --standalone` in a fresh home | empty, exit 0 | empty, exit 0 |
| `debug agents --standalone` mentions runner / e2e-tester / claude-mem | no | no |
| `api GET /api/plugin --standalone` | `data: []` | `data: []` |
| absolute notes instruction reaches the model (A) | yes | yes |
| workspace `AGENTS.md` reaches the model (B) | **no** | yes |
| workspace skill listed to the model (C) | **no** (only built-ins `opencode`, `report`) | yes (`sonda`) |
| "create prova.txt" → file created | no (`BLOCCATO`, zero tool calls) | no (`BLOCCATO`, zero tool calls) |
| `auth.json` in isolated data home | none | none |

A parent-directory `AGENTS.md` (marker P, in the directory above the workspace)
was **not** loaded in discovery mode, with or without `git init` in the workspace
(model-reported marker list: A, B, C only).

Q5: `harness2.core.proposer._opencode_stdout` and `_opencode_cost` from the
unmodified clone (`.env` opens blocked by an audit hook) parsed both captured runs:
text matched, error empty, cost 0.0. VERIFIED.

**Correction adopted for Tasks 4 and 7:** do NOT set `OPENCODE_DISABLE_PROJECT_CONFIG`.
With it, the rendered `AGENTS.md` and `.opencode/skills/` (the whole harness) are
silently ignored. Keep every other part of the Q1 recipe (allowlisted env, isolated
HOME/XDG/TMPDIR, `OPENCODE_CONFIG_DIR`, explicit `OPENCODE_CONFIG`,
`OPENCODE_DISABLE_CLAUDE_CODE=true`, `--standalone`), and keep absolute
`instructions` paths (they work in both modes). Task 7 adds a test that a rendered
workspace exposes the `triage` skill and the guardrail text to a real run
(integration-marked).
