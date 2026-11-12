"""Frozen cohorts, resumable improvement and explicit repository promotion."""

from dataclasses import replace
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import shutil
from types import SimpleNamespace

import pytest

from assistant.bench import improve
from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore
from assistant.bench.seed import SeedTurn, SeededTurn
from assistant.harness_rules import load_tree
from assistant.markers import load_markers
from assistant.sessions import ToolCall, Turn
from assistant.spend import record_usage

NOW = datetime(2026, 11, 11, 12, tzinfo=timezone.utc)
HARNESS = Path(__file__).resolve().parents[2] / "harness"


def turn(identifier, **kwargs):
    return replace(Turn(identifier, identifier, 0, NOW, [], "ciao", "ciao", [], None, [], "dev"), **kwargs)


def read(path):
    return json.loads(path.read_text())


@pytest.fixture
def rig(profile, bench_snapshot, monkeypatch):
    shutil.copytree(HARNESS, profile.root / "harness-live")
    seeds = [SeededTurn(**vars(turn(f"seed-{i}")), seed=SeedTurn(
        f"seed-{i}", "ciao", [], NOW.isoformat(), [], [], None, "train" if i < 4 else "holdout"))
        for i in range(8)]
    seed_db = profile.bench_dir / "seed/seed.db"
    seed_db.parent.mkdir(parents=True)
    shutil.copyfile(bench_snapshot, seed_db)
    monkeypatch.setattr(improve, "ensure_seed_snapshot", lambda p: seed_db)
    monkeypatch.setattr(improve, "build_seed_turns", lambda p, snapshot: seeds)
    real = []
    monkeypatch.setattr(improve, "_real_turns", lambda p: list(real))
    calls = []

    def propose(task_id, k, **kwargs):
        from harness2.core.schedule import Namespace

        calls.append((task_id, k, kwargs))
        directory = Namespace("operator", kwargs["adapter"].substrate.name, "agg", 1, tag=kwargs["tag"]).task_dir(
            kwargs["runs_root"], task_id) / "harnesses/composed1"
        if directory.exists():
            shutil.rmtree(directory)
        shutil.copytree(HARNESS, directory)
        path = directory / "systemprompt.md"
        path.write_text(path.read_text() + "\nUsa risposte brevi.\n")
        record_usage(profile, "fake:" + kwargs["tag"], datetime.now(timezone.utc),
                     profile.settings.model, 100, 20, 0.25)
        return SimpleNamespace(selections={"agg": {"ok": True, "chosen_label": "composed1"}})

    def evaluate(p, tree, turns, directory, **kwargs):
        assert kwargs["seed_snapshot"] == seed_db
        return [TurnScore(t.turn_id, dict.fromkeys(RUBRIC_DIMENSIONS, 8), [], 8, "") for t in turns]

    monkeypatch.setattr(improve, "run_task", propose)
    monkeypatch.setattr(improve.gate, "evaluate", evaluate)
    return SimpleNamespace(p=profile, seeds=seeds, real=real, calls=calls, seed_db=seed_db,
                           snapshot=bench_snapshot)


def test_select_batch_prefers_negative_then_actions():
    turns = [turn("new", at=NOW + timedelta(hours=1)),
             turn("action", tool_calls=[ToolCall("dwt", "snooze", {}, True, "")]),
             turn("negative", next_message="No, più breve per favore")]
    assert [t.turn_id for t in improve.select_batch(turns, dict.fromkeys([t.turn_id for t in turns], "train"))] == [
        "negative", "action", "new"]


def test_batch_at_most_25_and_all_when_fewer_than_15():
    turns = [turn(str(i)) for i in range(40)]
    splits = dict.fromkeys([t.turn_id for t in turns], "train")
    assert len(improve.select_batch(turns, splits)) == 25
    assert len(improve.select_batch(turns[:7], splits)) == 7


def test_splits_assigned_once_and_stable(rig):
    turns = [turn(str(i)) for i in range(500)]
    first = improve.assign_splits(rig.p, turns)
    assert 70 <= list(first.values()).count("holdout") <= 130
    assert improve.assign_splits(rig.p, list(reversed(turns))) == first
    assert read(rig.p.bench_dir / "splits.json")["sessions"] == first
    improve.assign_splits(rig.p, [turn("new")])
    assert read(rig.p.bench_dir / "splits.json")["sessions"].items() >= first.items()


def test_holdout_never_trained_even_after_eviction(rig):
    rig.real.extend(turn(str(i), at=NOW + timedelta(seconds=i)) for i in range(70))
    (rig.p.bench_dir / "splits.json").write_text(json.dumps(dict.fromkeys([t.turn_id for t in rig.real], "holdout")))
    regression = improve.update_regression_set(rig.p)
    assert len(regression) == 60 and "0" not in {t.turn_id for t in regression}
    assert improve.select_batch(rig.real, improve.assign_splits(rig.p, rig.real)) == []


def test_real_turns_split_by_session(rig):
    first = turn("session0:0", session_id="session0", message="PRIVATE HOLDOUT")
    second = turn("session0:1", session_id="session0", history=[("user", first.message)])
    splits = improve.assign_splits(rig.p, [first, second, *rig.seeds])
    assert splits[first.turn_id] == splits[second.turn_id] == "train"  # sha256(session0) % 5 == 1
    stored = read(rig.p.bench_dir / "splits.json")
    assert stored["sessions"]["session0"] == splits[first.turn_id]
    assert all(splits[t.turn_id] == t.seed.split for t in rig.seeds)
    assert improve.assign_splits(rig.p, [second])[second.turn_id] == splits[first.turn_id]


def test_mixed_legacy_session_never_exposed_to_training(rig):
    rig.real.extend([
        turn("session0:0", session_id="session0", message="PRIVATE HOLDOUT"),
        turn("session0:1", session_id="session0", history=[("user", "PRIVATE HOLDOUT")]),
    ])
    (rig.p.bench_dir / "splits.json").write_text(json.dumps({"session0:0": "holdout", "session0:1": "train"}))
    directory = improve.run_improvement(rig.p, NOW)
    state = read(directory / "run.json")
    assert not set(state["batch_ids"]) & {"session0:0", "session0:1"}
    assert "session0:0" in state["regression_ids"]
    adapter = rig.calls[0][2]["adapter"]
    assert "PRIVATE HOLDOUT" not in adapter.task_prompt(adapter.batch_id)
    evidence = adapter.task_files(adapter.batch_id)
    assert "PRIVATE HOLDOUT" not in (evidence / "reazioni.md").read_text()
    # A later export/eviction must not allow any turn of this session to train.
    later = turn("session0:2", session_id="session0", message="PRIVATE HOLDOUT")
    splits = improve.assign_splits(rig.p, [later])
    assert improve.select_batch([later], splits) == []


def test_run_state_written_before_paid_work(rig, monkeypatch):
    def crash(*args, **kwargs):
        markers = load_markers(rig.p)
        state = read(rig.p.bench_dir / "runs" / markers.improve_run / "run.json")
        assert state["batch_ids"] and state["regression_ids"] and state["live_fingerprint"]
        assert str(rig.seed_db) in read(rig.p.bench_dir / "pins.json")["snapshots"]
        raise RuntimeError("interrupted")

    monkeypatch.setattr(improve, "run_task", crash)
    with pytest.raises(RuntimeError, match="interrupted"):
        improve.run_improvement(rig.p, NOW)
    assert load_markers(rig.p).improve_run


def test_last_improve_start_set_before_work(rig, monkeypatch):
    def crash(p):
        assert load_markers(p).last_improve_start == NOW
        raise RuntimeError("before cohorts")

    monkeypatch.setattr(improve, "_real_turns", crash)
    with pytest.raises(RuntimeError, match="before cohorts"):
        improve.run_improvement(rig.p, NOW)
    assert load_markers(rig.p).last_improve_start == NOW


def test_no_real_turns_uses_seed_train_and_holdout(rig):
    directory = improve.run_improvement(rig.p, NOW)
    state = read(directory / "run.json")
    assert state["batch_ids"] == [t.turn_id for t in rig.seeds if t.seed.split == "train"]
    assert state["regression_ids"] == [t.turn_id for t in rig.seeds if t.seed.split == "holdout"]
    _, k, kwargs = rig.calls[0]
    assert k == 1 and kwargs["modes"] == ("agg",)
    assert kwargs["tag"] == "operator-" + directory.name
    assert kwargs["library"].root == rig.p.bench_dir / "library"
    assert kwargs["prop"].spec.model == rig.p.settings.model
    assert kwargs["prop"].spec.timeout == 1800
    assert kwargs["adapter"].seed_snapshot == rig.seed_db


@pytest.mark.parametrize("real_state", ["none", "holdout", "used"])
@pytest.mark.parametrize("max_batch", [3, 25])
def test_empty_real_train_falls_back_to_unused_seed_train(rig, real_state, max_batch):
    rig.seeds[:4] = reversed(rig.seeds[:4])
    if real_state != "none":
        rig.real.append(turn("real"))
        split = "holdout" if real_state == "holdout" else "train"
        (rig.p.bench_dir / "splits.json").write_text(json.dumps({"real": split}))
    if real_state == "used":
        previous = rig.p.bench_dir / "runs/previous"
        previous.mkdir(parents=True)
        (previous / "run.json").write_text(json.dumps({"batch_ids": ["real", "seed-0"]}))

    directory = improve.run_improvement(rig.p, NOW, max_batch=max_batch)
    state = read(directory / "run.json")
    expected = ["seed-1", "seed-2", "seed-3"] if real_state == "used" else ["seed-0", "seed-1", "seed-2", "seed-3"]
    assert state["batch_ids"] == expected[:max_batch]
    assert state["regression_ids"] == ["seed-4", "seed-5", "seed-6", "seed-7"] + (
        ["real"] if real_state == "holdout" else [])
    assert (directory / "GATE").read_text().strip() == "passed"
    assert "Nessun nuovo turno" not in (directory / "report.md").read_text()


def test_seed_train_used_once_then_exhausted(rig):
    rig.seeds[:4] = reversed(rig.seeds[:4])
    first = improve.run_improvement(rig.p, NOW, max_batch=3)
    second = improve.run_improvement(rig.p, NOW + timedelta(days=1), max_batch=3)
    exhausted = improve.run_improvement(rig.p, NOW + timedelta(days=2), max_batch=3)

    assert read(first / "run.json")["batch_ids"] == ["seed-0", "seed-1", "seed-2"]
    assert read(second / "run.json")["batch_ids"] == ["seed-3"]
    assert read(exhausted / "run.json")["batch_ids"] == []
    assert len(rig.calls) == 2
    assert (exhausted / "GATE").read_text().strip() == "failed"
    assert "Nessun nuovo turno di training disponibile" in (exhausted / "report.md").read_text()


def test_regression_set_capped_at_60_keeps_seed_holdout(rig):
    rig.real.extend(turn(str(i), at=NOW + timedelta(seconds=i)) for i in range(100))
    (rig.p.bench_dir / "splits.json").write_text(json.dumps(dict.fromkeys([t.turn_id for t in rig.real], "holdout")))
    regression = improve.update_regression_set(rig.p)
    assert len(regression) == 60
    assert {t.turn_id for t in rig.seeds if t.seed.split == "holdout"} <= {t.turn_id for t in regression}
    assert {t.turn_id for t in rig.real[-56:]} <= {t.turn_id for t in regression}


def test_improve_writes_report_candidate_gate_cost(rig):
    directory = improve.run_improvement(rig.p, NOW)
    assert (directory / "GATE").read_text().strip() == "passed"
    assert "Usa risposte brevi" in (directory / "candidate/systemprompt.md").read_text()
    report = (directory / "report.md").read_text()
    assert "$0.250000" in report and "Ho imparato:" in report and "brevita" in report
    assert (directory / "GATE").stat().st_mtime_ns >= (directory / "report.md").stat().st_mtime_ns
    assert load_markers(rig.p).improve_run is None
    assert read(rig.p.bench_dir / "pins.json")["runs"] == {}


def test_improve_resumes_unfinished_run(rig, monkeypatch):
    original = improve.run_task
    seen = []

    def crash(task_id, k, **kwargs):
        seen.append((task_id, kwargs["tag"], [t.turn_id for t in kwargs["adapter"].batch]))
        raise RuntimeError("sleep")

    monkeypatch.setattr(improve, "run_task", crash)
    with pytest.raises(RuntimeError):
        improve.run_improvement(rig.p, NOW)
    identifier = load_markers(rig.p).improve_run
    state = (rig.p.bench_dir / "runs" / identifier / "run.json").read_bytes()
    rig.real.append(turn("new-export"))
    rig.seeds.clear()
    monkeypatch.setattr(improve, "run_task", original)
    directory = improve.run_improvement(rig.p, NOW + timedelta(hours=1))
    task_id, _, kwargs = rig.calls[0]
    assert seen == [(task_id, kwargs["tag"], [t.turn_id for t in kwargs["adapter"].batch])]
    assert directory.name == identifier and (directory / "run.json").read_bytes() == state
    assert (directory / "GATE").read_text().strip() == "passed"


def test_evidence_frozen_before_paid_work_and_resume(rig, monkeypatch):
    from assistant.bench import judge

    seen, task_text, evaluations = [], [], []
    def evidence(turn, *, latest):
        seen.append(turn.turn_id)
        return "frozen:" + latest.name
    monkeypatch.setattr(judge, "evidence_for", evidence)
    original = improve.run_task
    def crash(*args, **kwargs):
        adapter = kwargs["adapter"]
        directory = rig.p.bench_dir / "runs" / load_markers(rig.p).improve_run
        frozen = read(directory / "evidence.json")
        assert set(frozen) == {t.turn_id for t in rig.seeds}
        task_text.append((adapter.task_files(adapter.batch_id) / "reazioni.md").read_text())
        raise RuntimeError("interrupted")
    monkeypatch.setattr(improve, "run_task", crash)
    with pytest.raises(RuntimeError, match="interrupted"):
        improve.run_improvement(rig.p, NOW)
    directory = rig.p.bench_dir / "runs" / load_markers(rig.p).improve_run
    frozen = (directory / "evidence.json").read_bytes()
    shutil.copy2(rig.snapshot, rig.snapshot.with_name("dwt-20261112T100000Z.db"))
    def resumed(*args, **kwargs):
        adapter = kwargs["adapter"]
        task_text.append((adapter.task_files(adapter.batch_id) / "reazioni.md").read_text())
        return original(*args, **kwargs)
    def evaluate(p, tree, turns, run, **kwargs):
        evaluations.append(kwargs["frozen_evidence"])
        return [TurnScore(t.turn_id, dict.fromkeys(RUBRIC_DIMENSIONS, 8), [], 8, "") for t in turns]
    monkeypatch.setattr(improve, "run_task", resumed)
    monkeypatch.setattr(improve.gate, "evaluate", evaluate)
    improve.run_improvement(rig.p, NOW + timedelta(hours=1))
    assert len(seen) == len(set(seen)) == len(rig.seeds)
    assert task_text[0] == task_text[1] and "dwt-20261112T083000Z.db" in task_text[0]
    assert (directory / "evidence.json").read_bytes() == frozen
    assert evaluations == [read(directory / "evidence.json")] * 2


def test_resume_rejects_training_session_quarantined_since_crash(rig, monkeypatch):
    rig.real.append(turn("session0:1", session_id="session0", history=[("user", "PRIVATE HOLDOUT")]))
    original = improve.run_task
    def crash(*args, **kwargs):
        raise RuntimeError("interrupted")
    monkeypatch.setattr(improve, "run_task", crash)
    with pytest.raises(RuntimeError, match="interrupted"):
        improve.run_improvement(rig.p, NOW)
    # An old split receipt becomes visible after the interrupted training call.
    (rig.p.bench_dir / "splits.json").write_text(json.dumps({"session0:0": "holdout", "session0:1": "train"}))
    monkeypatch.setattr(improve, "run_task", original)
    directory = improve.run_improvement(rig.p, NOW + timedelta(hours=1))
    assert (directory / "GATE").read_text().strip() == "failed"
    assert rig.calls == []


def test_changed_candidate_during_evaluation_fails_gate(rig, monkeypatch):
    original = improve.gate.evaluate
    def evaluate(p, tree, turns, run, **kwargs):
        if tree.name == "candidate":
            path = tree / "systemprompt.md"
            path.write_text(path.read_text() + "\nNon valutato.\n")
        return original(p, tree, turns, run, **kwargs)
    monkeypatch.setattr(improve.gate, "evaluate", evaluate)
    directory = improve.run_improvement(rig.p, NOW)
    assert (directory / "GATE").read_text().strip() == "failed"


def test_training_exposure_never_reused(rig):
    rig.real.extend(turn(str(i)) for i in range(5))
    (rig.p.bench_dir / "splits.json").write_text(json.dumps(dict.fromkeys([t.turn_id for t in rig.real], "train")))
    first = improve.run_improvement(rig.p, NOW)
    second = improve.run_improvement(rig.p, NOW + timedelta(days=1))
    exhausted = improve.run_improvement(rig.p, NOW + timedelta(days=2))
    assert read(first / "run.json")["batch_ids"] == [str(i) for i in range(5)]
    assert read(second / "run.json")["batch_ids"] == ["seed-0", "seed-1", "seed-2", "seed-3"]
    assert not read(exhausted / "run.json")["batch_ids"]
    assert len(rig.calls) == 2
    assert (exhausted / "GATE").read_text().strip() == "failed"


@pytest.mark.parametrize("problem", ["missing_selection", "invalid_tree", "incomplete", "changed_live"])
def test_improve_failures_never_promotable(rig, monkeypatch, problem):
    original = improve.run_task

    def propose(*args, **kwargs):
        result = original(*args, **kwargs)
        if problem == "missing_selection":
            result.selections = {}
        elif problem == "invalid_tree":
            for path in (rig.p.bench_dir / "h2/runs").rglob("guardrails.md"):
                path.write_text("weakened")
        elif problem == "changed_live":
            (rig.p.root / "harness-live/systemprompt.md").write_text("changed during run")
        return result

    monkeypatch.setattr(improve, "run_task", propose)
    if problem == "incomplete":
        def fail(*args, **kwargs):
            raise improve.gate.EvaluationIncomplete("missing judgment")
        monkeypatch.setattr(improve.gate, "evaluate", fail)
    directory = improve.run_improvement(rig.p, NOW)
    assert (directory / "GATE").read_text().strip() == "failed"
    assert "FAIL" in (directory / "report.md").read_text()
    with pytest.raises(ValueError):
        improve.promote(rig.p, directory.name, rig.p.root / "repo")


def test_promote_refuses_failed_gate(rig, tmp_path):
    directory = improve.run_improvement(rig.p, NOW)
    (directory / "GATE").write_text("failed\n")
    with pytest.raises(ValueError, match="gate|GATE"):
        improve.promote(rig.p, directory.name, tmp_path)


def test_promote_copies_validated_tree(rig, tmp_path):
    directory = improve.run_improvement(rig.p, NOW)
    repo = tmp_path / "repo"
    shutil.copytree(HARNESS, repo / "assistant/harness")
    removed = repo / "assistant/harness/skills/old/SKILL.md"
    removed.parent.mkdir(parents=True)
    removed.write_text("obsolete")
    changed = improve.promote(rig.p, directory.name, repo)
    assert removed in changed and repo / "assistant/harness/systemprompt.md" in changed
    assert load_tree(repo / "assistant/harness") == load_tree(directory / "candidate")
    assert improve.promote(rig.p, directory.name, repo) == []
    (directory / "candidate/scripts").mkdir()
    (directory / "candidate/scripts/evil.sh").write_text("evil")
    with pytest.raises(ValueError):
        improve.promote(rig.p, directory.name, repo)


def test_promote_refuses_candidate_changed_after_evaluation(rig, tmp_path):
    directory = improve.run_improvement(rig.p, NOW)
    repo = tmp_path / "repo"
    shutil.copytree(HARNESS, repo / "assistant/harness")
    before = load_tree(repo / "assistant/harness")
    prompt = directory / "candidate/systemprompt.md"
    prompt.write_text(prompt.read_text() + "\nUna modifica mai valutata.\n")
    with pytest.raises(ValueError, match="fingerprint|valutat"):
        improve.promote(rig.p, directory.name, repo)
    assert load_tree(repo / "assistant/harness") == before


def test_promote_refuses_legacy_pass_without_fingerprint(rig, tmp_path):
    directory = improve.run_improvement(rig.p, NOW)
    (directory / "gate.json").unlink(missing_ok=True)
    with pytest.raises(ValueError, match="GATE|fingerprint"):
        improve.promote(rig.p, directory.name, tmp_path)


def test_improve_soft_cap_warning(rig):
    p = replace(rig.p, settings=replace(rig.p.settings, soft_cap_usd=0.1))
    directory = improve.run_improvement(p, NOW)
    assert "soglia" in (directory / "report.md").read_text()


def test_improve_limits_small_run(rig):
    directory = improve.run_improvement(rig.p, NOW, max_batch=3, max_regression=3)
    state = read(directory / "run.json")
    assert len(state["batch_ids"]) == 3 and len(state["regression_ids"]) == 3


def test_improve_cli_runs_foreground(profile, monkeypatch, capsys):
    import subprocess
    import sys
    from assistant.cli import main

    calls = []
    def foreground(command, **kwargs):
        calls.append((command, kwargs))
        return subprocess.CompletedProcess(command, 1)
    monkeypatch.setattr(subprocess, "run", foreground)
    assert main(["bench", "improve", "--profile", "dev", "--state-root", str(profile.root.parent.parent)]) == 1
    assert calls[0][0] == [sys.executable, "-m", "assistant", "bench", "improve-worker", "--profile", "dev",
                           "--state-root", str(profile.root.parent.parent)]
    assert "capture_output" not in calls[0][1]


def test_promote_cli(rig, tmp_path, capsys):
    from assistant.cli import main

    directory = improve.run_improvement(rig.p, NOW)
    repo = tmp_path / "repo"
    shutil.copytree(HARNESS, repo / "assistant/harness")
    assert main(["bench", "promote", "--profile", "dev", "--state-root", str(rig.p.root.parent.parent),
                 "--run", directory.name, "--repo", str(repo)]) == 0
    assert "systemprompt.md" in capsys.readouterr().out


def test_crashed_improvement_respects_retry_interval(profile):
    from assistant.markers import Markers
    from assistant.tick import due_jobs

    markers = Markers(last_improve_start=NOW, improve_run="unfinished")
    assert "improve" not in due_jobs(markers, NOW + timedelta(minutes=15), profile.settings,
                                     ui_active=False, newest_snapshot_age=None)


def test_improve_reads_only_profile_exports(profile, tmp_path, monkeypatch):
    profile.exports_dir.mkdir()
    fixture = Path(__file__).resolve().parents[1] / "fixtures/session_export.json"
    shutil.copyfile(fixture, profile.exports_dir / "session.json")
    (profile.exports_dir / "index.json").write_text("{}")
    other = tmp_path / "elsewhere.json"
    shutil.copyfile(fixture, other)
    turns = improve._real_turns(profile)
    assert turns and all(t.profile == "dev" for t in turns)
    def mixed(path, name):
        assert path == profile.exports_dir / "session.json" and name == "dev"
        return [turn("dev-turn"), turn("operator-turn", profile="operator")]
    monkeypatch.setattr(improve, "turns_from_export", mixed)
    assert [t.turn_id for t in improve._real_turns(profile)] == ["dev-turn"]


def test_improve_unpins_only_batch_and_keeps_regression(rig, monkeypatch):
    snapshot = rig.snapshot.with_name("dwt-20261110T083000Z.db")
    rig.snapshot.rename(snapshot)
    training_snapshot = snapshot.with_name("dwt-20261111T000000Z.db")
    shutil.copyfile(snapshot, training_snapshot)
    rig.real.extend([turn("train"), turn("holdout", at=NOW - timedelta(days=1))])
    (rig.p.bench_dir / "splits.json").write_text(json.dumps({"train": "train", "holdout": "holdout"}))
    external = str(rig.p.snapshots_dir / "external.db")
    (rig.p.bench_dir / "pins.json").write_text(json.dumps({"snapshots": [external]}))
    original = improve.run_task
    def pinned(*args, **kwargs):
        pins = read(rig.p.bench_dir / "pins.json")
        assert str(training_snapshot) in pins["runs"][load_markers(rig.p).improve_run]
        return original(*args, **kwargs)
    monkeypatch.setattr(improve, "run_task", pinned)
    improve.run_improvement(rig.p, NOW)
    pins = read(rig.p.bench_dir / "pins.json")
    assert pins["runs"] == {} and str(snapshot) in pins["regression"]
    assert external in pins["snapshots"] and str(training_snapshot) not in pins["snapshots"]


def test_improve_recovers_after_gate_before_marker_clear(rig, monkeypatch):
    original = improve._finish
    monkeypatch.setattr(improve, "_finish", lambda *args: None)
    directory = improve.run_improvement(rig.p, NOW)
    assert load_markers(rig.p).improve_run == directory.name
    monkeypatch.setattr(improve, "_finish", original)
    assert improve.run_improvement(rig.p, NOW + timedelta(hours=1)) == directory
    assert len(rig.calls) == 1 and load_markers(rig.p).improve_run is None


def test_improve_regression_decline_fails_gate(rig, monkeypatch):
    def evaluate(p, tree, turns, directory, **kwargs):
        value = 9 if tree.name == "harness-live" else 8
        return [TurnScore(t.turn_id, dict.fromkeys(RUBRIC_DIMENSIONS, value), [], value, "") for t in turns]
    monkeypatch.setattr(improve.gate, "evaluate", evaluate)
    directory = improve.run_improvement(rig.p, NOW)
    assert (directory / "GATE").read_text().strip() == "failed"
    assert "8.00" in (directory / "report.md").read_text()
