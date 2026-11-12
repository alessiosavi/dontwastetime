"""Backups preserve WAL data and obey retention without touching live state."""

from datetime import datetime, timedelta, timezone
import json
import os
import sqlite3

import pytest


NOW = datetime(2026, 11, 12, 10, tzinfo=timezone.utc)


def source_db(profile):
    profile.dwt_state_dir.mkdir(parents=True, exist_ok=True)
    db = sqlite3.connect(profile.dwt_state_dir / "dwt.db")
    db.execute("pragma journal_mode=wal")
    db.execute("create table mail (text)")
    db.execute("insert into mail values ('synthetic mail')")
    db.commit()
    return db


def test_snapshot_is_consistent_copy(profile):
    from assistant.snapshot import take_snapshot

    source = source_db(profile)
    try:
        snapshot = take_snapshot(profile, NOW)
        assert snapshot.name == "dwt-20261112T100000Z.db"
        with sqlite3.connect(snapshot) as db:
            assert db.execute("select text from mail").fetchall() == [("synthetic mail",)]
        assert not list(profile.snapshots_dir.glob("*.tmp"))
    finally:
        source.close()


def test_snapshot_reused_within_60_min(profile):
    from assistant.snapshot import take_snapshot, snapshot_before

    with source_db(profile):
        first = take_snapshot(profile, NOW)
        assert take_snapshot(profile, NOW + timedelta(minutes=59, seconds=59)) == first
        second = take_snapshot(profile, NOW + timedelta(minutes=60))
    assert second != first
    assert snapshot_before(profile, NOW - timedelta(seconds=1)) is None
    assert snapshot_before(profile, NOW + timedelta(minutes=59)) == first
    assert snapshot_before(profile, NOW + timedelta(minutes=60)) == second


def test_prune_drops_older_than_56_days(profile):
    from assistant.snapshot import prune_snapshots

    profile.snapshots_dir.mkdir()
    for name in ("dwt-20260916T095959Z.db", "dwt-20260917T100000Z.db", "unrelated.db"):
        (profile.snapshots_dir / name).touch()
    assert prune_snapshots(profile, NOW) == 1
    assert sorted(p.name for p in profile.snapshots_dir.iterdir()) == [
        "dwt-20260917T100000Z.db", "unrelated.db",
    ]


def test_prune_keeps_pinned_and_seed(profile):
    from assistant.snapshot import prune_snapshots

    profile.snapshots_dir.mkdir()
    pinned = profile.snapshots_dir / "dwt-20260101T000000Z.db"
    pinned.write_text("pinned")
    seed = profile.bench_dir / "seed/seed.db"
    seed.parent.mkdir(parents=True)
    seed.write_text("seed")
    (profile.bench_dir / "pins.json").write_text(json.dumps({"snapshots": [pinned.name]}))
    assert prune_snapshots(profile, NOW) == 0
    assert pinned.read_text() == "pinned"
    assert seed.read_text() == "seed"


def test_ui_active_rejects_stale_and_invalid_pid(profile):
    from assistant.snapshot import ui_active

    assert not ui_active(profile)
    pid = profile.root / "ui.pid"
    pid.write_text(str(os.getpid()))
    assert ui_active(profile)
    for value in ("not a pid", "0", "-1", "999999999"):
        pid.write_text(value)
        assert not ui_active(profile)


def test_snapshot_rejects_naive_time_and_missing_source(profile):
    from assistant.snapshot import take_snapshot, snapshot_before, prune_snapshots

    for function in (take_snapshot, snapshot_before, prune_snapshots):
        with pytest.raises(ValueError):
            function(profile, NOW.replace(tzinfo=None))
    with pytest.raises(sqlite3.OperationalError):
        take_snapshot(profile, NOW)
    assert not (profile.dwt_state_dir / "dwt.db").exists()


def test_snapshot_cli(profile, capsys):
    from assistant.cli import main

    source = source_db(profile)
    try:
        assert main(["snapshot", "--profile", "dev", "--state-root",
                     str(profile.root.parents[1])]) == 0
    finally:
        source.close()
    assert len(capsys.readouterr().out.splitlines()) == 1
    assert len(list(profile.snapshots_dir.glob("dwt-*.db"))) == 1
