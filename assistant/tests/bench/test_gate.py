"""Promotion and Phase 0 gates fail closed on incomplete evaluations."""

from dataclasses import replace
import math
import shutil
from threading import Barrier, Lock

import pytest

from assistant.bench.checks import RolloutTurn
from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore


def score(identifier="t", value=8.0, *, dims=None, failures=()):
    values = dict.fromkeys(RUBRIC_DIMENSIONS, value) | (dims or {})
    return TurnScore(identifier, values, list(failures), sum(values.values()) / 5, "PRIVATE REPLY")


def test_gate_passes_equal_scores():
    from assistant.bench.gate import regression_gate

    result = regression_gate([score("a"), score("b", 7)], [score("b", 7), score("a")])
    assert result.passed and result.live_mean == result.cand_mean == 7.5
    assert result.dim_drops == dict.fromkeys(RUBRIC_DIMENSIONS, 0)
    assert result.new_failures == []


def test_gate_fails_on_dimension_drop_over_1():
    from assistant.bench.gate import regression_gate

    result = regression_gate([score()], [score(value=9, dims={"lingua_tono": 6.9})])
    assert not result.passed and result.cand_mean > result.live_mean
    assert result.dim_drops["lingua_tono"] == pytest.approx(1.1)
    assert regression_gate([score()], [score(value=9, dims={"lingua_tono": 7})]).passed


def test_gate_fails_on_new_deterministic_failure():
    from assistant.bench.gate import regression_gate

    live = [score("a", failures=["missing_action"]), score("b")]
    assert regression_gate(live, live).passed
    candidate = [score("a"), score("b", failures=["missing_action"])]
    result = regression_gate(live, candidate)
    assert not result.passed and len(result.new_failures) == 1
    assert "b" in result.new_failures[0] and "missing_action" in result.new_failures[0]
    result = regression_gate([live[0]], [score("a", failures=["unknown_reference"])])
    assert not result.passed and "unknown_reference" in result.new_failures[0]


def test_gate_fails_when_candidate_mean_lower():
    from assistant.bench.gate import regression_gate

    result = regression_gate([score()], [score(value=7.99)])
    assert not result.passed and all(drop < 1 for drop in result.dim_drops.values())


def test_seed_bar_requires_mean_7_and_no_failures():
    from assistant.bench.gate import seed_bar

    assert seed_bar([score(value=6), score("b", 8)])
    assert not seed_bar([score(value=6.99)])
    assert not seed_bar([score(value=10, failures=["note_from_mail"])])
    assert not seed_bar([])
    assert not seed_bar([score(value=math.nan)])


@pytest.mark.parametrize("live,cand", [
    ([], []), ([score()], []), ([score("a")], [score("b")]),
    ([score(), score()], [score()]), ([score()], [score(), score()]),
    ([score()], [score(value=math.inf)]), ([score(value=math.nan)], [score()]),
    ([score()], [replace(score(), dims={"fatti": 8})]),
    ([score()], [replace(score(), dims=score(dims={"fatti": math.nan}).dims)]),
    ([score()], [replace(score(), mean=math.nan)]),
])
def test_gate_rejects_mismatched_or_empty_cohorts(live, cand):
    from assistant.bench.gate import regression_gate

    with pytest.raises(ValueError):
        regression_gate(live, cand)


def test_evaluate_raises_on_judge_none(profile, bench_turn, bench_snapshot, tmp_path, monkeypatch):
    from assistant.bench import gate, judge, rollout

    monkeypatch.setattr(rollout, "execute_turns", lambda *a, **k: [
        RolloutTurn(bench_turn.turn_id, "risposta", [], tmp_path)])
    monkeypatch.setattr(judge, "judge_turn", lambda *a, **k: None)
    with pytest.raises(gate.EvaluationIncomplete):
        gate.evaluate(profile, tmp_path, [bench_turn], tmp_path / "run")


@pytest.mark.parametrize("kind", ["empty", "missing", "wrong-id", "bad-score", "duplicate", "collision"])
def test_evaluate_rejects_incomplete_cohorts(profile, bench_turn, bench_snapshot, tmp_path, monkeypatch, kind):
    from assistant.bench import gate, judge, rollout

    turns = [] if kind == "empty" else [bench_turn]
    if kind == "duplicate":
        turns *= 2
    if kind == "collision":
        turns.append(replace(bench_turn, turn_id=bench_turn.turn_id.replace(":", "_")))
    monkeypatch.setattr(rollout, "execute_turns", lambda *a, **k: [] if kind == "missing" else [
        RolloutTurn("other" if kind == "wrong-id" else bench_turn.turn_id, "risposta", [], tmp_path)])
    monkeypatch.setattr(judge, "judge_turn", lambda *a, **k: score(bench_turn.turn_id, math.nan))
    with pytest.raises(gate.EvaluationIncomplete):
        gate.evaluate(profile, tmp_path, turns, tmp_path / "run")


def test_evaluate_parallel_order_latest_and_seed_arguments(profile, bench_turn, bench_snapshot, tmp_path, monkeypatch):
    from assistant.bench import gate, judge, rollout
    from assistant.bench.seed import SeedTurn

    latest = profile.snapshots_dir / "dwt-20261112T100000Z.db"
    shutil.copy2(bench_snapshot, latest)
    turns = [replace(bench_turn, turn_id=f"turn:{i}") for i in range(4)]
    seed = SeedTurn("turn:0", "ciao", [], "snapshot", [], [], None, "holdout")
    seeds = {seed.id: seed}
    barrier, lock = Barrier(2), Lock()
    active, maximum = 0, 0

    def execute(p, tree, batch, run_dir, **kwargs):
        nonlocal active, maximum
        assert p == profile and tree == tmp_path and run_dir == tmp_path / "run"
        assert kwargs == {"seeds": seeds, "seed_snapshot": bench_snapshot}
        assert len(batch) == 1
        with lock:
            active += 1
            maximum = max(maximum, active)
        barrier.wait(timeout=5)
        return [RolloutTurn(batch[0].turn_id, "risposta", [], run_dir / rollout._turn_name(batch[0].turn_id))]

    def evidence(turn, *, latest):
        assert latest.name == "dwt-20261112T100000Z.db"
        return turn.turn_id

    def grade(p, turn, result, text, **kwargs):
        nonlocal active
        assert result.turn_id == text == turn.turn_id
        assert kwargs.get("seed") == seeds.get(turn.turn_id)
        with lock:
            active -= 1
        return score(turn.turn_id)

    monkeypatch.setattr(rollout, "execute_turns", execute)
    monkeypatch.setattr(judge, "evidence_for", evidence)
    monkeypatch.setattr(judge, "judge_turn", grade)
    result = gate.evaluate(profile, tmp_path, turns, tmp_path / "run", seeds=seeds,
                           seed_snapshot=bench_snapshot, jobs=2)
    assert [s.turn_id for s in result] == [t.turn_id for t in turns]
    assert maximum == 2 and active == 0


def test_evaluate_rejects_invalid_jobs_before_work(profile, bench_turn, tmp_path):
    from assistant.bench.gate import evaluate

    with pytest.raises(ValueError):
        evaluate(profile, tmp_path, [bench_turn], tmp_path, jobs=0)
