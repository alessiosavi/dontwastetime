"""Replay isolation, resumption and event/accounting contracts."""

from dataclasses import replace
from datetime import timedelta
import hashlib
import json
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys

import pytest


HARNESS = Path(__file__).resolve().parents[2] / "harness"


def execute(profile, turns, run_dir, **kwargs):
    from assistant.bench.rollout import execute_turns

    return execute_turns(profile, HARNESS, turns, run_dir, **kwargs)


def test_execute_turns_isolates_each_turn(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    event_runner(mutate=True)
    before = (bench_snapshot.stat().st_mtime_ns, hashlib.sha256(bench_snapshot.read_bytes()).hexdigest())
    profile.memory_dir.mkdir()
    (profile.memory_dir / "notes.md").write_text("live memory")
    other = replace(bench_turn, turn_id="session/1", at=bench_turn.at + timedelta(minutes=1))
    results = execute(profile, [bench_turn, other], tmp_path / "run")
    assert [r.run_dir.name for r in results] == ["session_0", "session_1"]
    for result in results:
        with sqlite3.connect(result.run_dir / "dwt/dwt.db") as db:
            assert db.execute("SELECT value FROM synthetic").fetchone() == ("changed",)
        assert (result.run_dir / "ws/memory").resolve() == result.run_dir / "notes"
        assert (result.run_dir / "ws/memory/notes.md").read_text() == ""
        assert result.tool_calls[0].server == "notes"
        assert result.tool_calls[0].tool == "echo_note"
        assert result.tool_calls[0].ok
        assert result.tool_calls[0].output == "NOTE:synthetic-task1"
        assert result.reply.endswith("DONE")
        assert all((result.run_dir / f).is_file() for f in
                   ("events.jsonl", "reply.md", "tool_calls.json", "done.json"))
    assert before == (bench_snapshot.stat().st_mtime_ns, hashlib.sha256(bench_snapshot.read_bytes()).hexdigest())
    assert (profile.memory_dir / "notes.md").read_text() == "live memory"


def test_execute_turns_passes_now(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    log = event_runner()
    result, = execute(profile, [bench_turn], tmp_path / "run")
    config = json.loads((result.run_dir / "ws/opencode.json").read_text())
    command = config["mcp"]["servers"]["dwt"]["command"]
    assert command[-2:] == ["--now", bench_turn.at.isoformat()]
    assert command[command.index("--state-dir") + 1] == str(result.run_dir / "dwt")
    invocation = json.loads(log.read_text())
    assert invocation["argv"][-2:] == ["--", "Conversazione precedente:\nOperator: ciao\n"
                                      "Assistente: Buongiorno\n\nMessaggio di Operator:\ncosa devo fare oggi?"]
    assert "--variant" not in invocation["argv"] and "--dir" not in invocation["argv"]


def test_execute_turns_resumable(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    from assistant.bench.rollout import execute_turns

    log = event_runner()
    tree = tmp_path / "tree"
    shutil.copytree(HARNESS, tree)
    run = tmp_path / "run"
    first, = execute_turns(profile, tree, [bench_turn], run)
    marker = first.run_dir / "done.json"
    original = marker.read_bytes()
    resumed, = execute_turns(profile, tree, [bench_turn], run)
    assert resumed == first
    assert len(log.read_text().splitlines()) == 1
    assert marker.read_bytes() == original
    (tree / "systemprompt.md").write_text((tree / "systemprompt.md").read_text() + "\nSii chiara.\n")
    execute_turns(profile, tree, [bench_turn], run)
    assert len(log.read_text().splitlines()) == 2
    assert marker.read_bytes() != original
    # Snapshot identity changes invalidate the completion marker too.
    with sqlite3.connect(bench_snapshot) as db:
        db.execute("INSERT INTO synthetic VALUES ('new')")
    execute_turns(profile, tree, [bench_turn], run)
    assert len(log.read_text().splitlines()) == 3


def test_partial_turn_without_done_is_rerun(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    log = event_runner()
    partial = tmp_path / "run/session_0"
    partial.mkdir(parents=True)
    (partial / "reply.md").write_text("partial")
    (partial / "old").write_text("stale")
    result, = execute(profile, [bench_turn], partial.parent)
    assert result.reply != "partial"
    assert not (partial / "old").exists()
    assert len(log.read_text().splitlines()) == 1


def test_execute_turns_use_ops_store(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    log = event_runner()
    execute(profile, [bench_turn], tmp_path / "run")
    invocation = json.loads(log.read_text())
    assert invocation["env"]["XDG_DATA_HOME"] == str(profile.root / "opencode-ops/data")
    assert invocation["env"]["OPENCODE_CONFIG"] == str(tmp_path / "run/session_0/ws/opencode.json")
    usage, = [json.loads(line) for line in (profile.root / "usage.jsonl").read_text().splitlines()]
    assert usage["call_id"] == "rollout:run:session:0:ses_e32d1389bffeWkff0K3fth8DPG"
    assert (usage["tokens_in"], usage["tokens_out"], usage["tokens_cache_read"]) == (1420, 77, 1536)


def test_stub_turn_uses_stub_dwt_command(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    from assistant.bench.seed import SeedTurn

    event_runner()
    seed = SeedTurn(bench_turn.turn_id, "ciao", [], "snapshot", [], [], "injection", "train")
    # Explicit seed snapshot wins even if the turn predates every stored snapshot.
    turn = replace(bench_turn, at=bench_turn.at - timedelta(days=1))
    result, = execute(profile, [turn], tmp_path / "run", seeds={turn.turn_id: seed},
                      seed_snapshot=bench_snapshot)
    config = json.loads((result.run_dir / "ws/opencode.json").read_text())
    assert config["mcp"]["servers"]["dwt"]["command"] == [
        sys.executable, "-m", "assistant.bench.stub_dwt", "--stub", "injection"]


@pytest.mark.parametrize("failure", ["exit", "timeout"])
def test_failed_rollout_is_incomplete_not_retried(profile, bench_turn, bench_snapshot, tmp_path,
                                                  event_runner, monkeypatch, failure):
    from assistant import opencode_env
    from assistant.bench.judge import judge_turn

    log = event_runner(exit_code=7)
    if failure == "timeout":
        def timeout(*args, **kwargs):
            raise subprocess.TimeoutExpired("opencode", 600, output=b'{"type":"step_finish",'
                b'"part":{"tokens":{"input":9,"output":2}}}\n')
        monkeypatch.setattr(opencode_env, "run_prompt", timeout)
    result, = execute(profile, [bench_turn], tmp_path / "run")
    resumed, = execute(profile, [bench_turn], tmp_path / "run")
    assert result.reply == resumed.reply == ""
    assert json.loads((result.run_dir / "done.json").read_text())["failed"] is True
    monkeypatch.setattr(opencode_env, "run_prompt", lambda *a, **k: pytest.fail("failed rollout must not call judge"))
    score = judge_turn(profile, bench_turn, result, "")
    assert score is None
    assert len(log.read_text().splitlines()) == 1 if failure == "exit" else not log.exists()
    usage = json.loads((profile.root / "usage.jsonl").read_text())
    assert usage["tokens_in"] == (1420 if failure == "exit" else 9)


def test_rollout_without_snapshot_is_skipped(profile, bench_turn, tmp_path, event_runner, caplog):
    log = event_runner()
    assert execute(profile, [bench_turn], tmp_path / "run") == []
    assert "session:0" in caplog.text
    assert not log.exists()


def test_tool_use_events_match_export_parsing():
    from assistant.bench.rollout import run_events
    from assistant.sessions import ToolCall, tool_call_from_part

    states = [
        ({"status": "completed", "input": {"id": 12}, "content": [{"type": "text", "text": "structured"}], "output": "fallback"}, {"id": 12}, True, "structured"),
        ({"status": "completed", "input": {}, "output": "x" * 5000}, {}, True, "x" * 4000),
        ({"status": "completed", "error": "failed", "output": "error"}, {}, False, "error"),
        ({"status": "completed", "isError": True, "output": "error"}, {}, False, "error"),
        ({"status": "running"}, {}, False, ""),
    ]
    for name, server, tool in (("dwt_get_message", "dwt", "get_message"),
                               ("notes_list_notes", "notes", "list_notes"), ("read", "", "read")):
        for state, args, ok, output in states:
            exported = tool_call_from_part({"name": name, "state": state})
            _, calls, _, failed = run_events(json.dumps({"type": "tool_use", "part": {"tool": name, "state": state}}))
            expected = ToolCall(server, tool, args, ok, output)
            assert not failed and calls == [expected] and exported == expected


def test_done_is_written_last(profile, bench_turn, bench_snapshot, tmp_path, event_runner, monkeypatch):
    from assistant.bench import rollout

    event_runner()
    write = rollout._atomic_write
    written = []

    def inspect(path, text):
        written.append(path.name)
        if path.name == "done.json":
            assert all((path.parent / name).exists() for name in ("events.jsonl", "reply.md", "tool_calls.json"))
        write(path, text)

    monkeypatch.setattr(rollout, "_atomic_write", inspect)
    execute(profile, [bench_turn], tmp_path / "run")
    assert written == ["events.jsonl", "reply.md", "tool_calls.json", "done.json"]


def test_safe_turn_names_cannot_overwrite_one_another(profile, bench_turn, bench_snapshot, tmp_path, event_runner):
    event_runner()
    with pytest.raises(ValueError, match="conflitto"):
        execute(profile, [bench_turn, replace(bench_turn, turn_id="session/0")], tmp_path / "run")


@pytest.mark.parametrize("with_session", [True, False])
def test_crash_rerun_records_both_paid_rollout_sessions(profile, bench_turn, bench_snapshot, tmp_path,
                                                       event_runner, monkeypatch, with_session):
    from assistant.bench import rollout

    def output(session):
        return json.dumps({"type": "step_finish", **({"sessionID": session} if with_session else {}),
                           "part": {"tokens": {"input": 9, "output": 2}}}) + "\n"
    event_runner(stdout=output("ses_first"))
    original = rollout._atomic_write
    def crash(path, text):
        if path.name == "done.json":
            raise RuntimeError("crash after usage, before cache commit")
        original(path, text)
    monkeypatch.setattr(rollout, "_atomic_write", crash)
    with pytest.raises(RuntimeError, match="crash after usage"):
        execute(profile, [bench_turn], tmp_path / "run")
    monkeypatch.setattr(rollout, "_atomic_write", original)
    event_runner(stdout=output("ses_second"))
    execute(profile, [bench_turn], tmp_path / "run")
    rows = [json.loads(line) for line in (profile.root / "usage.jsonl").read_text().splitlines()]
    assert len(rows) == 2 and sum(row["tokens_in"] for row in rows) == 18
    assert len({row["call_id"] for row in rows}) == 2
    if with_session:
        assert [row["call_id"] for row in rows] == [
            "rollout:run:session:0:ses_first", "rollout:run:session:0:ses_second"]
