"""Opt-in paid call on an installed dev profile; Claude runs this check."""

import math
import os
from pathlib import Path

import pytest


REAL_PATH = os.environ.get("PATH", "")
STATE_ROOT = os.environ.get("ASSISTANT_IT_STATE_ROOT")


@pytest.mark.integration
@pytest.mark.skipif(not STATE_ROOT, reason="ASSISTANT_IT_STATE_ROOT must point at an installed dev profile")
def test_one_seed_turn_end_to_end(monkeypatch):
    from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore, judge_turn
    from assistant.bench.rollout import execute_turns
    from assistant.bench.seed import build_seed_turns
    from assistant.profile import load_profile
    from assistant.snapshot import _snapshots

    monkeypatch.setenv("PATH", REAL_PATH)
    p = load_profile("dev", state_root=Path(STATE_ROOT))
    snapshots = _snapshots(p)
    if not snapshots:
        pytest.skip("installed dev profile has no snapshot")
    snapshot = snapshots[-1][1]
    turns = build_seed_turns(p, snapshot)
    turn = next((t for t in turns if t.seed.stub is None), None)
    assert turn is not None, "no seed turn resolved on the installed dev snapshot"
    tree = Path(__file__).resolve().parents[2] / "harness"
    rollout, = execute_turns(p, tree, [turn], p.bench_dir / "it-task14", seed_snapshot=snapshot)
    assert rollout.reply.strip(), "seed rollout failed or returned an empty reply"
    score = judge_turn(p, turn, rollout, "", seed=turn.seed)
    assert isinstance(score, TurnScore)
    assert set(score.dims) == set(RUBRIC_DIMENSIONS)
    assert all(math.isfinite(value) and 0 <= value <= 10 for value in score.dims.values())
