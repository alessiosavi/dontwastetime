"""The seed CLI evaluates the live tree against a durable database copy."""

from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import json
from pathlib import Path
import shutil
import sqlite3

import pytest

from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore
from assistant.bench.seed import SeedTurn, SeededTurn


def test_seed_db_created_once_and_reused(profile, bench_snapshot):
    from assistant.bench.seed import ensure_seed_snapshot
    from assistant.snapshot import prune_snapshots

    newest = profile.snapshots_dir / "dwt-20261112T100000Z.db"
    shutil.copy2(bench_snapshot, newest)
    with sqlite3.connect(newest) as db:
        db.execute("UPDATE synthetic SET value='newest'")
    with ThreadPoolExecutor(max_workers=4) as pool:
        copies = list(pool.map(lambda _: ensure_seed_snapshot(profile), range(8)))
    path = profile.bench_dir / "seed/seed.db"
    assert copies == [path] * 8
    before = path.read_bytes(), path.stat().st_mtime_ns
    with sqlite3.connect(path) as db:
        assert db.execute("SELECT value FROM synthetic").fetchone() == ("newest",)
    with sqlite3.connect(newest) as db:
        db.execute("UPDATE synthetic SET value='changed'")
    assert ensure_seed_snapshot(profile) == path
    assert (path.read_bytes(), path.stat().st_mtime_ns) == before
    assert prune_snapshots(profile, datetime(2027, 2, 1, tzinfo=timezone.utc)) == 2
    assert path.is_file()
    assert ensure_seed_snapshot(profile) == path
    assert (path.read_bytes(), path.stat().st_mtime_ns) == before


def test_seed_db_takes_snapshot_when_none_exists(profile):
    from assistant.bench.seed import ensure_seed_snapshot

    profile.dwt_state_dir.mkdir(parents=True)
    with sqlite3.connect(profile.dwt_state_dir / "dwt.db") as db:
        db.execute("CREATE TABLE synthetic (value TEXT)")
        db.execute("INSERT INTO synthetic VALUES ('source')")
    path = ensure_seed_snapshot(profile)
    with sqlite3.connect(path) as db:
        assert db.execute("SELECT value FROM synthetic").fetchone() == ("source",)
    assert len(list(profile.snapshots_dir.glob("dwt-*.db"))) == 1


def test_seed_db_preserves_source_snapshot_clock(profile, bench_snapshot):
    from assistant.bench.seed import _snapshot_time, ensure_seed_snapshot

    path = ensure_seed_snapshot(profile)
    expected = datetime(2026, 11, 12, 8, 30, tzinfo=timezone.utc)
    assert _snapshot_time(path) == expected
    assert _snapshot_time(ensure_seed_snapshot(profile)) == expected


@pytest.mark.parametrize("split", ["all", "train", "holdout"])
@pytest.mark.parametrize("outcome", ["pass", "fail", "incomplete"])
def test_seed_cli_dispatch_report_and_exit(profile, bench_turn, bench_snapshot, monkeypatch, capsys, split, outcome):
    from assistant import cli, spend
    from assistant.bench import gate, seed

    templates = [SeedTurn(name, "PRIVATE MAIL", [], "snapshot", [], [], None, partition)
                 for name, partition in [("a", "train"), ("b", "holdout"), ("skipped", "holdout")]]
    turns = [SeededTurn(**bench_turn.__dict__ | {"turn_id": item.id, "seed": item}) for item in templates[:2]]
    monkeypatch.setattr(seed, "load_seed_turns", lambda: templates)

    def build(p, snapshot):
        assert snapshot == profile.bench_dir / "seed/seed.db" and snapshot.is_file()
        return turns

    monkeypatch.setattr(seed, "build_seed_turns", build)
    seen = []

    def evaluate(p, tree, batch, directory, **kwargs):
        assert p == profile and tree == profile.root / "harness-live"
        expected = [t for t in turns if split == "all" or t.seed.split == split]
        assert batch == expected
        assert kwargs["jobs"] == 2 and kwargs["seed_snapshot"] == profile.bench_dir / "seed/seed.db"
        assert kwargs["seeds"] == {t.turn_id: t.seed for t in expected}
        assert directory.parent == profile.bench_dir / "seed-runs" and directory.is_dir()
        seen.append(directory)
        if outcome == "incomplete":
            raise gate.EvaluationIncomplete("Giudizio non disponibile")
        return [TurnScore(t.turn_id, dict.fromkeys(RUBRIC_DIMENSIONS, 8.0),
                          ["note_from_mail"] if outcome == "fail" else [], 8.0, "PRIVATE REPLY") for t in batch]

    monkeypatch.setattr(gate, "evaluate", evaluate)

    def cost(p, start, end):
        assert p == profile and start.tzinfo and end >= start
        return spend.SpendTotal(0, 42)

    monkeypatch.setattr(spend, "spend_between", cost)
    code = cli.main(["bench", "seed", "--state-root", str(profile.root.parents[1]),
                     "--split", split, "--jobs", "2"])
    assert code == (0 if outcome == "pass" else 1)
    output = capsys.readouterr().out
    report = seen[0] / "report.md"
    assert output.rstrip().endswith(str(report))
    assert ("PASS" if code == 0 else "FAIL") in output.splitlines()[-1]
    text = report.read_text()
    assert "42" in text and "non disponibile" in text and "PRIVATE" not in text
    assert ("skipped" in text) == (split != "train")
    if outcome != "incomplete":
        for turn in turns:
            if split == "all" or turn.seed.split == split:
                assert f"{turn.turn_id}: 8.00" in output
    else:
        assert "incompleta" in text


@pytest.mark.parametrize("position", ["global", "bench", "seed"])
def test_seed_cli_allows_operator_and_respects_profile_position(profile, monkeypatch, position):
    from assistant import cli, snapshot as snapshots
    from assistant.bench import gate, seed

    profiles = []

    def snapshot(p):
        profiles.append(p.name)
        return p.root / "seed.db"

    monkeypatch.setattr(seed, "ensure_seed_snapshot", snapshot)
    monkeypatch.setattr(seed, "build_seed_turns", lambda *a: [])
    monkeypatch.setattr(seed, "load_seed_turns", lambda: [])
    monkeypatch.setattr(snapshots, "_snapshots", lambda p: [(None, p.root / "latest.db")])
    evaluations = []
    def incomplete(*a, **k):
        evaluations.append(a[0].name)
        raise gate.EvaluationIncomplete("Nessun turno")
    monkeypatch.setattr(gate, "evaluate", incomplete)
    args = {"global": ["--profile", "operator", "bench", "seed"],
            "bench": ["bench", "--profile", "operator", "seed"],
            "seed": ["bench", "seed", "--profile", "operator"]}[position]
    assert cli.main([*args, "--state-root", str(profile.root.parents[1])]) == 1
    assert profiles == ["operator"]
    assert evaluations == ["operator"]


def test_seed_cli_rejects_zero_jobs_before_snapshot(monkeypatch, capsys):
    from assistant.cli import main
    from assistant.bench import seed

    monkeypatch.setattr(seed, "ensure_seed_snapshot", lambda *a: pytest.fail("invalid CLI started a snapshot"))
    with pytest.raises(SystemExit) as result:
        main(["bench", "seed", "--jobs", "0"])
    assert result.value.code == 2
    assert "jobs deve essere un intero positivo" in capsys.readouterr().err


def test_seed_cli_real_pipeline_with_fake_model(profile, bench_snapshot, event_runner, monkeypatch, capsys):
    from assistant import cli
    from assistant.bench import seed

    shutil.copytree(Path(__file__).resolve().parents[2] / "harness",
                    profile.root / "harness-live")
    templates = [SeedTurn("stub-" + split, "ciao", [], "snapshot", [], [], "injection", split)
                 for split in ("train", "holdout")]
    monkeypatch.setattr(seed, "load_seed_turns", lambda: templates)
    grade = json.dumps(dict.fromkeys(RUBRIC_DIMENSIONS, 7) | {"reasons": "PRIVATE JUDGE REASONS"})
    event_runner(json.dumps({"type": "text", "part": {"text": grade}}) + "\n" + json.dumps({
        "type": "step_finish", "part": {"tokens": {"input": 10, "output": 20}, "cost": 0}}) + "\n")
    assert cli.main(["bench", "seed", "--state-root", str(profile.root.parents[1]), "--jobs", "2"]) == 0
    output = capsys.readouterr().out
    assert "stub-train: 7.00" in output and "stub-holdout: 7.00" in output
    report = next((profile.bench_dir / "seed-runs").glob("*/report.md")).read_text()
    assert "PASS" in report and "120" in report and "PRIVATE" not in report
    # Retention may remove all ordinary snapshots, but the seed stays frozen;
    # the next invocation must refresh only the snapshot used as latest evidence.
    seed_db = profile.bench_dir / "seed/seed.db"
    before = seed_db.read_bytes(), seed_db.stat().st_mtime_ns
    profile.dwt_state_dir.mkdir(parents=True)
    shutil.copy2(bench_snapshot, profile.dwt_state_dir / "dwt.db")
    for snapshot in profile.snapshots_dir.glob("*.db"):
        snapshot.unlink()
    assert cli.main(["bench", "seed", "--state-root", str(profile.root.parents[1]), "--jobs", "2"]) == 0
    assert (seed_db.read_bytes(), seed_db.stat().st_mtime_ns) == before
    assert len(list(profile.snapshots_dir.glob("dwt-*.db"))) == 1
