"""Replay a candidate against private per-turn database and notes copies."""

from dataclasses import asdict
from datetime import datetime, timezone
import hashlib
import json
import logging
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

from assistant import opencode_env
from assistant.bench.checks import RolloutTurn
from assistant.bench.seed import SeedTurn, _copy_snapshot
from assistant.harness_rules import load_tree
from assistant.install import _atomic_write
from assistant.notes_mcp import _require_aware
from assistant.profile import Profile
from assistant.render import render_workspace
from assistant.sessions import ToolCall, Turn, tool_call_from_part
from assistant.snapshot import snapshot_before
from assistant.spend import record_usage


logger = logging.getLogger(__name__)


def transcript(turn: Turn) -> str:
    history = "\n".join(f"{'Operator' if role == 'user' else 'Assistente'}: {text}"
                        for role, text in turn.history)
    return (("Conversazione precedente:\n" + history + "\n\n") if history else "") + (
        "Messaggio di Operator:\n" + turn.message)


def run_events(stdout: str | bytes | None, *, session_ids: set[str] | None = None
               ) -> tuple[str, list[ToolCall], dict, bool]:
    """Parse 2.2.7 JSONL, retaining partial usage even after a failed run."""
    if isinstance(stdout, bytes):
        stdout = stdout.decode("utf-8", errors="replace")
    texts, calls, seen = [], [], set()
    usage = {"tokens_in": 0, "tokens_out": 0, "tokens_cache_read": 0, "cost_usd": 0.0}
    failed = False
    for line in (stdout or "").splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
            kind, part = event["type"], event.get("part", {})
            session = event.get("sessionID") or part.get("sessionID")
            if session_ids is not None and isinstance(session, str) and session:
                session_ids.add(session)
            if kind == "text":
                text = part["text"]
                if not isinstance(text, str):
                    raise TypeError("invalid text event")
                texts.append(text)
            elif kind == "tool_use":
                calls.append(tool_call_from_part(part))
            elif kind == "error":
                failed = True
            elif kind == "step_finish":
                key = part.get("id")
                if key and key in seen:
                    continue
                if key:
                    seen.add(key)
                tokens = part.get("tokens", {})
                usage["tokens_in"] += tokens.get("input", 0)
                usage["tokens_out"] += tokens.get("output", 0)
                usage["tokens_cache_read"] += tokens.get("cache", {}).get("read", 0)
                usage["cost_usd"] += part.get("cost") or 0.0
        except (ValueError, KeyError, TypeError, AttributeError):
            failed = True
    return "\n".join(texts).strip(), calls, usage, failed


def _turn_name(turn_id: str) -> str:
    name = turn_id.replace(":", "_").replace("/", "_")
    if name in ("", ".", "..") or "\x00" in name:
        raise ValueError("Identificativo del turno non valido.")
    return name


def _fingerprint(tree_fingerprint: str, turn_id: str, snapshot: Path) -> str:
    info = snapshot.stat()
    identity = [tree_fingerprint, turn_id, snapshot.name, info.st_size, info.st_mtime_ns]
    return hashlib.sha256(json.dumps(identity, ensure_ascii=False).encode()).hexdigest()


def execute_turns(
    p: Profile, tree_dir: Path, turns: list[Turn], run_dir: Path, *,
    seeds: dict[str, SeedTurn] | None = None, seed_snapshot: Path | None = None,
) -> list[RolloutTurn]:
    tree = load_tree(tree_dir)
    tree_fingerprint = hashlib.sha256(json.dumps(sorted(tree.items()), ensure_ascii=False).encode()).hexdigest()
    run_dir = run_dir.resolve()
    names = [_turn_name(turn.turn_id) for turn in turns]
    if len(set(names)) != len(names):
        raise ValueError("Identificativi dei turni in conflitto dopo la normalizzazione.")
    results = []
    for turn, name in zip(turns, names, strict=True):
        _require_aware(turn.at)
        seed = (seeds or {}).get(turn.turn_id) or getattr(turn, "seed", None)
        snapshot = seed_snapshot if seed is not None else snapshot_before(p, turn.at)
        if snapshot is None:
            logger.warning("Turno %s saltato: nessuna snapshot precedente disponibile.", turn.turn_id)
            continue
        fingerprint = _fingerprint(tree_fingerprint, turn.turn_id, snapshot)
        directory = run_dir / name
        if directory.is_symlink():
            raise ValueError("La directory del turno non può essere un collegamento simbolico.")
        try:
            done = json.loads((directory / "done.json").read_text(encoding="utf-8"))
            if done.get("fingerprint") == fingerprint:
                calls = [ToolCall(**call) for call in json.loads((directory / "tool_calls.json").read_text(encoding="utf-8"))]
                results.append(RolloutTurn(turn.turn_id, (directory / "reply.md").read_text(encoding="utf-8"), calls, directory))
                continue
        except (FileNotFoundError, ValueError, TypeError, AttributeError):
            pass
        if directory.exists():
            shutil.rmtree(directory)
        state = directory / "dwt"
        state.mkdir(mode=0o700, parents=True)
        _copy_snapshot(snapshot, state / "dwt.db")
        command = ([sys.executable, "-m", "assistant.bench.stub_dwt", "--stub", seed.stub]
                   if seed and seed.stub else None)
        workspace = render_workspace(p, tree_dir, dest=directory / "ws", dwt_state_dir=state,
                                     now=turn.at, notes_dir=directory / "notes", dwt_command=command)
        at = datetime.now(timezone.utc)
        stdout, failed = "", False
        try:
            result = opencode_env.run_prompt(p, "ops", transcript(turn), cwd=workspace,
                                             timeout=600, model=p.settings.model)
            stdout, failed = result.stdout, result.returncode != 0
        except Exception as error:
            failed = True
            if isinstance(error, subprocess.TimeoutExpired):
                stdout = error.stdout or ""
            logger.warning("Rollout %s fallito (%s).", turn.turn_id, type(error).__name__)
        if isinstance(stdout, bytes):
            stdout = stdout.decode("utf-8", errors="replace")
        sessions = set()
        reply, calls, usage, event_failure = run_events(stdout, session_ids=sessions)
        failed = failed or event_failure
        # Cache identity stays stable; paid invocations do not. A transport
        # failure may precede even the first event carrying a session id.
        invocation = ",".join(sorted(sessions)) if sessions else uuid.uuid4().hex
        record_usage(p, f"rollout:{run_dir.name}:{turn.turn_id}:{invocation}", at, p.settings.model, **usage)
        if failed:
            reply = ""
        _atomic_write(directory / "events.jsonl", stdout)
        _atomic_write(directory / "reply.md", reply)
        _atomic_write(directory / "tool_calls.json", json.dumps([asdict(call) for call in calls], ensure_ascii=False) + "\n")
        # This is the commit marker: never publish it before the other artifacts.
        _atomic_write(directory / "done.json", json.dumps({"fingerprint": fingerprint, "failed": failed}) + "\n")
        results.append(RolloutTurn(turn.turn_id, reply, calls, directory))
    return results
