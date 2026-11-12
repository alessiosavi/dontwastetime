"""Synthetic dwt MCP: canned read results, recorded success-looking writes.

Run: python -m assistant.bench.stub_dwt --stub injection
Optional --record-calls writes successful triage ToolCall records as JSONL.
No database, mail service, model, or live dwt process is used by this server.
"""

import argparse
from copy import deepcopy
from dataclasses import asdict
import inspect
import json
from pathlib import Path
import re
from typing import Annotated, Any

from mcp.server.fastmcp import FastMCP
from pydantic import Field

from assistant.bench.seed import DATA_DIR, READ_TOOLS
from assistant.sessions import TRIAGE_TOOLS, ToolCall


class StubDataset:
    def __init__(self, name: str, record_calls: Path | None = None):
        if not re.fullmatch(r"[a-z][a-z0-9_-]*", name):
            raise ValueError("Nome dei dati di prova non valido.")
        self.data = json.loads((DATA_DIR / "stubs" / f"{name}.json").read_text(encoding="utf-8"))
        self.calls: list[ToolCall] = []
        self.record_calls = record_calls

    def call(self, tool: str, args: dict | None = None) -> dict:
        args = args or {}
        if tool in TRIAGE_TOOLS:
            result = {"thread": str(args.get("thread", "")), "state": {
                "mark_handled": "handled", "snooze": "snoozed", "mute_thread": "muted",
                "reopen": "due", "relabel": "due",
            }[tool], "due_at": None, "snoozed_until": args.get("until") or None}
            if tool == "relabel":
                result = {"old": "unsure", "new": args["bucket"], "state": "due"}
            call = ToolCall("dwt", tool, dict(args), True, json.dumps(result, ensure_ascii=False))
            self.calls.append(call)
            if self.record_calls:
                with self.record_calls.open("a", encoding="utf-8") as output:
                    output.write(json.dumps(asdict(call), ensure_ascii=False) + "\n")
            return result
        if tool not in READ_TOOLS:
            raise ValueError("Strumento dwt sconosciuto.")
        result = deepcopy(self.data[tool])
        if tool in ("needs_reply", "list_messages", "search"):
            items = result["items"]
            for key in ("mailbox", "bucket", "state", "priority", "direction", "has_attachments"):
                if args.get(key) is not None and args.get(key) != "":
                    items = [item for item in items if item.get(key) == args[key]]
            if args.get("from"):
                items = [item for item in items if item["untrusted"]["from"] == args["from"]]
            if tool == "search":
                words = args.get("query", "").casefold().split()
                items = [item for item in items if all(word in " ".join(
                    str(value) for value in item["untrusted"].values()).casefold() for word in words)]
            result["items"] = items[:args.get("limit") or 20]
        elif tool in ("get_message", "get_thread"):
            all_messages = self.data["get_thread"]["messages"]
            if tool == "get_message":
                message = next((item for item in all_messages if item["id"] == args.get("id")), None)
                if message is None:
                    raise ValueError("Messaggio di prova non trovato.")
                result.update(deepcopy(message))
            else:
                ref = str(args.get("thread", ""))
                message = next((item for item in all_messages if ref in (item["thread"], f'#{item["id"]}')), None)
                if message is None:
                    raise ValueError("Conversazione di prova non trovata.")
                result["thread"] = message["thread"]
                result["messages"] = [deepcopy(item) for item in all_messages if item["thread"] == message["thread"]]
        return result


# Explicit dwt input contracts from internal/mcpserver/tools.go. A generated
# signature avoids fourteen wrappers while preserving required args and `from`
# (a Python keyword) in the MCP schema. FastMCP passes aliases to **arguments.
_REQUIRED = inspect.Parameter.empty
_PAGE = [("limit", int, 20), ("cursor", str, "")]
_RANGE = [("since", str, ""), ("until", str, "")]
_MAILBOX = [("mailbox", str, "")]
_THREAD = [("thread", str, _REQUIRED)]
_INPUTS = {
    "context": [],
    "overview": _MAILBOX + _RANGE,
    "needs_reply": _MAILBOX + [("priority", str, ""), ("bucket", str, ""), ("state", str, "")] + _PAGE,
    "list_messages": _MAILBOX + [("bucket", str, ""),
                                 ("from_", Annotated[str, Field(alias="from")], ""),
                                 ("domain", str, ""), ("direction", str, ""),
                                 ("has_attachments", bool | None, None)] + _RANGE + _PAGE,
    "search": [("query", str, _REQUIRED)] + _MAILBOX + [("bucket", str, ""), ("order", str, "relevance")] + _RANGE + _PAGE,
    "get_thread": _THREAD + _PAGE,
    "get_message": [("id", int, _REQUIRED)],
    "digest": [("date", str, ""), ("since", str, "")],
    "health": [],
    "relabel": [("id", int, _REQUIRED), ("bucket", str, _REQUIRED)],
    "mark_handled": _THREAD + [("inbound_id", int, _REQUIRED), ("note", str, "")],
    "mute_thread": _THREAD,
    "snooze": _THREAD + [("inbound_id", int, _REQUIRED), ("until", str, _REQUIRED)],
    "reopen": _THREAD,
}


def create_server(stub: str, *, record_calls: Path | None = None) -> FastMCP:
    dataset = StubDataset(stub, record_calls)
    server = FastMCP("dwt")

    def register(name, parameters):
        def invoke(**arguments) -> dict[str, Any]:
            return dataset.call(name, arguments)

        invoke.__name__ = name
        invoke.__signature__ = inspect.Signature([
            inspect.Parameter(key, inspect.Parameter.POSITIONAL_OR_KEYWORD, default=default, annotation=kind)
            for key, kind, default in parameters
        ], return_annotation=dict[str, Any])
        server.add_tool(invoke, name=name, description=f"Dati sintetici dwt: {name}.")

    for name, parameters in _INPUTS.items():
        register(name, parameters)
    return server


def main(argv=None) -> None:
    parser = argparse.ArgumentParser(description="Server dwt con soli dati sintetici.")
    parser.add_argument("--stub", required=True)
    parser.add_argument("--record-calls", type=Path)
    args = parser.parse_args(argv)
    create_server(args.stub, record_calls=args.record_calls).run(transport="stdio")


if __name__ == "__main__":
    main()
