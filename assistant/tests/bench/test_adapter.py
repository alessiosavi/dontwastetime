"""Harness²'s exact adapter seam, including complete scoring cohorts."""

from dataclasses import replace
import json
from pathlib import Path
import shutil

import pytest

from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore
from assistant.bench.seed import SeedTurn


HARNESS = Path(__file__).resolve().parents[2] / "harness"


def make_adapter(profile, turns, seeds=None):
    from assistant.bench.adapter import OperatorAdapter

    return OperatorAdapter(profile, turns, seeds or {}, batch_id="batch-15")


def test_adapter_satisfies_protocol(profile, bench_turn):
    from assistant.h2 import harness2

    harness2()
    from harness2.core.spec import BenchmarkAdapter

    adapter = make_adapter(profile, [bench_turn])
    assert isinstance(adapter, BenchmarkAdapter)
    assert adapter.spec.name == "operator"
    assert adapter.spec.install_mode == "workspace_agents_md"
    assert not adapter.spec.allow_subagents
    assert "rispondere a Operator in italiano usando dwt" in adapter.spec.prompts.work
    assert adapter.spec.channels["proposer"].model == profile.settings.model
    assert adapter.spec.channels["proposer"].timeout == 1800
    assert adapter.substrate.name == "oc"
    assert adapter.tasks() == adapter.tasks("train") == ["batch-15"]
    assert adapter.domain_of("batch-15") == "operator"
    assert adapter.base_harness() == profile.root / "harness-live"
    assert "Messaggio di Operator:\ncosa devo fare oggi?" in adapter.task_prompt("batch-15")
    assert "Operator: ciao" in adapter.task_prompt("batch-15")


def test_adapter_task_files_include_only_batch_evidence(profile, bench_turn, bench_snapshot, monkeypatch):
    from assistant.bench import judge

    seen = []
    later = profile.snapshots_dir / "dwt-20261112T100000Z.db"
    shutil.copy2(bench_snapshot, later)
    def evidence(turn, *, latest):
        seen.append((turn.turn_id, latest))
        return "Operator ha detto: più breve"
    monkeypatch.setattr(judge, "evidence_for", evidence)
    adapter = make_adapter(profile, [bench_turn])
    directory = adapter.task_files("batch-15")
    text = "\n".join(path.read_text() for path in directory.rglob("*.md"))
    assert bench_turn.turn_id in text and "Operator ha detto: più breve" in text
    assert seen == [(bench_turn.turn_id, later)]


def test_adapter_executes_candidate_and_reads_artifacts(profile, bench_turn, bench_snapshot,
                                                       event_runner, tmp_path):
    event_runner()
    candidate = tmp_path / "candidate"
    shutil.copytree(HARNESS, candidate)
    (candidate / "systemprompt.md").write_text((candidate / "systemprompt.md").read_text() + "\nCANDIDATE_15\n")
    adapter = make_adapter(profile, [bench_turn])
    run_dir = adapter.execute("batch-15", candidate, "operator-oc-agg1/batch-15/base1", timeout=600)
    assert run_dir == profile.bench_dir / "h2/runs/operator-oc-agg1/batch-15/base1"
    assert "CANDIDATE_15" in (run_dir / "session_0/ws/AGENTS.md").read_text()
    assert adapter.delivered(run_dir)
    files = adapter.deliverable_files(run_dir)
    assert list(files) == ["session_0/reply.md"]
    assert "DONE" in files["session_0/reply.md"]
    assert bench_turn.turn_id in adapter.deliverable_text(run_dir)
    assert "DONE" in adapter.deliverable_text(run_dir)
    trajectory = adapter.trajectory(run_dir, obs_trunc=10_000)
    assert "[ACTION] notes_echo_note" in trajectory and "[OBSERVATION] NOTE:synthetic-task1" in trajectory
    assert bench_turn.turn_id in trajectory
    (run_dir / "session_0/reply.md").unlink()
    assert not adapter.delivered(run_dir)


def test_adapter_uses_base_and_seed_snapshot(profile, bench_turn, bench_snapshot, event_runner):
    event_runner()
    shutil.copytree(HARNESS, profile.root / "harness-live")
    seed = SeedTurn(bench_turn.turn_id, "cosa devo fare oggi?", [], "snapshot", [], [], None, "train")
    adapter = make_adapter(profile, [bench_turn], {bench_turn.turn_id: seed})
    run_dir = adapter.execute("batch-15", None, "seed/base1")
    assert adapter.delivered(run_dir)
    assert (run_dir / "session_0/dwt/dwt.db").is_file()


def test_adapter_validate_tree_uses_local_rules(profile, bench_turn):
    from assistant.harness_rules import load_tree

    adapter = make_adapter(profile, [bench_turn])
    tree = load_tree(HARNESS)
    adapter.validate_tree(tree)
    with pytest.raises(ValueError, match="guardrails.md"):
        adapter.validate_tree({**tree, "guardrails.md": ""})


@pytest.mark.parametrize("judge_missing", [False, True])
def test_adapter_score_uses_judge_and_frozen_evidence(profile, bench_turn, bench_snapshot,
                                                     event_runner, monkeypatch, judge_missing):
    from assistant.bench import judge

    event_runner()
    second = replace(bench_turn, turn_id="session:1", message="grazie")
    turns = [bench_turn, second]
    monkeypatch.setattr(judge, "evidence_for", lambda turn, *, latest: f"{turn.turn_id}:{latest.name}")
    adapter = make_adapter(profile, turns)
    run_dir = adapter.execute("batch-15", HARNESS, "scores/base1")
    later = profile.snapshots_dir / "dwt-20261112T100000Z.db"
    shutil.copy2(bench_snapshot, later)
    seen = []
    def grade(p, turn, rollout, evidence, *, seed=None):
        seen.append((turn.turn_id, rollout.reply, evidence))
        if judge_missing and turn is second:
            return None
        dims = dict(zip(RUBRIC_DIMENSIONS, [8, 7, 6, 9, 5] if turn is bench_turn else [7] * 5))
        return TurnScore(turn.turn_id, dims, [], sum(dims.values()) / 5, "Motivi")
    monkeypatch.setattr(judge, "judge_turn", grade)
    assert adapter.score(run_dir, "batch-15") == (None if judge_missing else {
        "n_passed": 8, "n_criteria": 10, "mean": 7.0})
    assert len(seen) == 2
    assert all(reply.endswith("DONE") and evidence.endswith(bench_snapshot.name) for _, reply, evidence in seen)


def test_adapter_score_rejects_missing_and_empty_cohorts(profile, bench_turn, bench_snapshot,
                                                       event_runner, monkeypatch):
    from assistant.bench import judge

    event_runner()
    adapter = make_adapter(profile, [bench_turn])
    run_dir = adapter.execute("batch-15", HARNESS, "incomplete/base1")
    (run_dir / "session_0/tool_calls.json").unlink()
    monkeypatch.setattr(judge, "judge_turn", lambda *a, **kw: pytest.fail("incomplete cohort must not be graded"))
    assert adapter.score(run_dir, "batch-15") is None
    assert make_adapter(profile, []).score(run_dir, "batch-15") is None
    assert not make_adapter(profile, []).delivered(run_dir)


def test_adapter_rejects_wrong_task_and_escaping_run_dir(profile, bench_turn):
    adapter = make_adapter(profile, [bench_turn])
    with pytest.raises(ValueError):
        adapter.execute("batch-15", HARNESS, "../outside")
    with pytest.raises(ValueError):
        adapter.task_prompt("holdout")
