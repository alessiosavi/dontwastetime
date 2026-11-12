"""Incremental UI-only exports and turn extraction for OpenCode 2.2.7."""

from dataclasses import dataclass
from datetime import datetime, timezone
import json
from pathlib import Path
import re

from assistant.install import _atomic_write
from assistant.opencode_env import run_opencode
from assistant.profile import Profile, _read_object


TRIAGE_TOOLS = ("relabel", "mark_handled", "mute_thread", "snooze", "reopen")


@dataclass(frozen=True)
class ToolCall:
    server: str
    tool: str
    args: dict
    ok: bool
    output: str


@dataclass
class Turn:
    turn_id: str
    session_id: str
    index: int
    at: datetime
    history: list[tuple[str, str]]
    message: str
    reply: str
    tool_calls: list[ToolCall]
    next_message: str | None
    following_tool_calls: list[ToolCall]
    profile: str


def _opencode_json(p: Profile, args: list[str]):
    result = run_opencode(p, "ui", args, cwd=p.workspace, timeout=120)
    if result.returncode:
        raise RuntimeError(f"OpenCode {args[0]} {args[1]}: uscita {result.returncode}")
    return json.loads(result.stdout)


def export_new_sessions(p: Profile) -> list[Path]:
    """Use session list --format json (verified in 2.2.7), not its 100-row default.

    run_opencode supplies --standalone and the isolated UI store to both commands.
    An interrupted run leaves a valid old index; extra exports are safe to repeat.
    """
    sessions = _opencode_json(p, ["session", "list", "--format", "json", "--max-count", "2147483647"])
    index_path = p.exports_dir / "index.json"
    index = _read_object(index_path)
    exported = []
    for session in sessions:
        sid, updated = session["id"], session["updated"]
        if not re.fullmatch(r"[A-Za-z0-9_-]+", sid) or sid == "index":
            raise ValueError("Identificativo di sessione OpenCode non valido.")
        path = p.exports_dir / f"{sid}.json"
        if index.get(sid) == updated and path.is_file():
            continue
        data = _opencode_json(p, ["session", "export", sid])
        if data["info"]["id"] != sid or not isinstance(data["messages"], list):
            raise ValueError("Esportazione OpenCode non valida.")
        _atomic_write(path, json.dumps(data, ensure_ascii=False, indent=2) + "\n")
        index[sid] = updated
        exported.append(path)
    _atomic_write(index_path, json.dumps(index, ensure_ascii=False, indent=2) + "\n")
    return exported


def _text(content: list[dict]) -> str:
    return "\n".join(part.get("text", "") for part in content if part.get("type") == "text")


def _tool_call(part: dict) -> ToolCall:
    server, separator, tool = part["name"].partition("_")
    if not separator:
        server, tool = "", server
    state = part.get("state", {})
    output = _text(state.get("content", []))
    if not output:
        output = state.get("output", "")
    return ToolCall(
        server, tool, state.get("input", {}),
        state.get("status") == "completed" and not state.get("error") and not state.get("isError"),
        output[:4000],
    )


def turns_from_export(path: Path, profile: str) -> list[Turn]:
    data = json.loads(path.read_text(encoding="utf-8"))
    session_id = data["info"]["id"]
    turns = []
    history = []
    for message in data["messages"]:
        if message["type"] == "user":
            if turns:
                previous = turns[-1]
                history.extend([("user", previous.message), ("assistant", previous.reply)])
            index = len(turns)
            turns.append(Turn(
                f"{session_id}:{index}", session_id, index,
                datetime.fromtimestamp(message["time"]["created"] / 1000, timezone.utc),
                list(history), message.get("text", ""), "", [], None, [], profile,
            ))
        elif message["type"] == "assistant" and turns:
            current = turns[-1]
            text = _text(message.get("content", []))
            if text:
                current.reply = "\n".join(filter(None, (current.reply, text)))
            current.tool_calls.extend(
                _tool_call(part) for part in message.get("content", []) if part["type"] == "tool"
            )
    for current, following in zip(turns, turns[1:]):
        current.next_message = following.message
        current.following_tool_calls = [
            call for call in following.tool_calls
            if call.server == "dwt" and call.tool in TRIAGE_TOOLS and call.ok
        ]
    return turns
