# OpenCode 2.2.7 fixtures

Captured locally on 2026-11-12 using `/opt/homebrew/bin/opencode` v2.2.7 and
`azure-ai/DeepSeek-V4.1-Flash-1017`. No mail, dwt, developer service, developer config,
or developer credential store was used. Only a synthetic stdio notes stub was enabled.

- `run_events.jsonl`: stdout from one `run --format json`; six events, one
  `tool_use` for `notes_echo_note`, completed with `NOTE:synthetic-task1`.
- `session_export.json`: unmodified session export shape; two user turns, three
  assistant messages, two idle markers, and exactly one tool call total.
- Session ID: `ses_e32d1389bffeWkff0K3fth8DPG`. The second turn requests a file write and receives
  `SECOND_TURN_BLOCKED`. The file did not exist before or after.

The fixture deliberately captures the **negative relative-instructions case**:
with project discovery disabled, `instructions:["memory/notes.md"]` does not resolve
from the workspace. The assistant reports it cannot see the marker. A later separate
run using an absolute instruction path returned `NOTES_MARKER_7b6a42`. See Q2 in
`../../docs/opencode-2.2.7.md`. Do not mistake this fixture's assistant text for a
successful marker-load check. Its purpose is to preserve real event/export shapes.

## Environment and config

Use Q1's exact allowlisted environment and cwd under
`/tmp/dwt-dev/task1/scratch/final`. The explicit config is
`workspace/opencode.json`; `opencode-config/` is empty. All HOME, XDG and temporary
paths are under scratch. Child credentials are supplied solely via AZURE_API_KEY;
the config has the literal `{env:AZURE_API_KEY}` expression. No auth.json exists and
the isolated credential table has zero rows.

The config was made from the authorized sanitized file
`/tmp/dwt-dev/azure-ai-provider.json`, preserving its provider/model
fields, then adding these fields:

```json
{
  "providers": {
    "azure-ai": {
      "settings": {
        "apiKey": "{env:AZURE_API_KEY}"
      }
    }
  },
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
  ],
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
  },
  "instructions": [
    "memory/notes.md"
  ],
  "update": "disable",
  "share": "disabled"
}
```
The `providers` entry above is a patch fragment, not a replacement for the supplied
provider's package/baseURL/model configuration. `memory/notes.md` contained exactly:

```text
Always-loaded synthetic marker: NOTES_MARKER_7b6a42.
```

## Stub server

The only MCP implementation was
`/tmp/dwt-dev/task1/scratch/echo_mcp.py`, with this complete source:

```python
import json, sys
for line in sys.stdin:
 try:
  q=json.loads(line)
  if 'id' not in q: continue
  method=q.get('method')
  if method=='initialize':
   result={'protocolVersion':'2024-11-05','capabilities':{'tools':{}},'serverInfo':{'name':'notes-stub','version':'1'}}
  elif method=='tools/list':
   result={'tools':[{'name':'echo_note','description':'Echo a synthetic note. No filesystem or network access.','inputSchema':{'type':'object','properties':{'text':{'type':'string'}},'required':['text'],'additionalProperties':False}}]}
  elif method=='tools/call':
   result={'content':[{'type':'text','text':'NOTE:'+q['params']['arguments']['text']}]}
  elif method=='ping': result={}
  else:
   print(json.dumps({'jsonrpc':'2.0','id':q['id'],'error':{'code':-32601,'message':'Unknown method'}}),flush=True);continue
  print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':result}),flush=True)
 except (ValueError,KeyError): pass
```
## Commands and captures

These are the exact argv captured by `scratch/probe.py`. Every command used the Q1
environment, cwd `scratch/final/workspace`, closed stdin and a 120-second timeout.
The first capture used the relative instructions entry above, before it was changed
to an absolute path for the separate positive Q2 probe.

```sh
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 'This is a synthetic fixture. State the always-loaded marker from memory/notes.md without reading a file. Then call notes echo_note exactly once with text synthetic-task1. Finally say DONE. No other tools.'
/opt/homebrew/bin/opencode run --standalone --auto --format json --model azure-ai/DeepSeek-V4.1-Flash-1017 --session ses_e32d1389bffeWkff0K3fth8DPG 'Create permission-proof.txt in the current workspace containing CREATED. Try a real filesystem write using edit, write, patch, or shell/bash. Do not use the notes echo tool. If writing is impossible, reply SECOND_TURN_BLOCKED. Be brief.'
/opt/homebrew/bin/opencode session export ses_e32d1389bffeWkff0K3fth8DPG --standalone
```
All three exited 0. Only stdout, not wrapper metadata or stderr, was copied:

```text
scratch/evidence/canonical-one.stdout -> run_events.jsonl
scratch/evidence/canonical-export.stdout -> session_export.json
```

The JSONL contains one call ID, so a consumer must count calls, not assume one
step or one assistant message per user turn. The fixture preserves actual IDs,
timestamps, token counts, tool state and reasoning fields. Home paths were scrubbed
by replacing the developer home path with `/Users/dev`; neither fixture contained a home path
requiring replacement. Scratch workspace paths remain, because they are synthetic,
not developer-home paths. No secret values or pairing URLs were saved in either
fixture. No manual data or fabricated cost was inserted; all measured costs are 0.

## Actual Harness² parser check

Command (exit 0):

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=/path/to/harness2 python3 /tmp/dwt-dev/task1/scratch/harness_parse.py
```

The complete importer/guard (no module monkeypatches) was:

```python
import json, sys
from pathlib import Path
blocked=[]
def guard(event,args):
 if event=='open' and isinstance(args[0],(str,bytes)):
  p=Path(args[0])
  if p.name=='.env' or p.name.startswith('.env.'):
   blocked.append(str(p));raise PermissionError('Spike prohibits .env reads')
sys.addaudithook(guard)
from harness2.core.proposer import _opencode_stdout,_opencode_cost
from harness2 import config
s=Path('/tmp/dwt-dev/task1/scratch/evidence/canonical-one.stdout').read_text()
text,error=_opencode_stdout(s)
print(json.dumps({'import_from':str(sys.modules['harness2.core.proposer'].__file__),'text':text,'error':error,'cost':_opencode_cost(s),'ROOT':str(config.ROOT),'BUN':str(config.BUN),'BUN_exists':config.BUN.exists(),'blocked_env_reads':blocked},indent=2))
```
Observed result:

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
The audit guard blocks Harness²'s automatic `.env` read before the file is opened;
its loader catches PermissionError. Harness² source and checkout remain read-only.
The input used here is byte-for-byte the captured source of `run_events.jsonl`.

## Structural verification

`python3 scratch/verify_deliverables.py` checks JSON/JSONL parsing, exactly one
successful stub call in both files, two user turns, matching session/call IDs,
no extra tools, zero costs, no secret/home-path leakage, and all nine documented
question statuses. It also re-runs the actual Harness² parser helpers on the final
fixture through the guarded importer. Results are reported with exit status; this
is fixture verification, not a product test suite. Claude's independent Q1/Q3/Q5
reruns remain pending.
