"""Wall-clock catch-up and job isolation, without mail, models or launchd."""

from datetime import date, datetime, timedelta, timezone
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
from types import SimpleNamespace
from zoneinfo import ZoneInfo

import pytest

from assistant.markers import Markers, load_markers, save_markers
from assistant.profile import Settings
from assistant.status import pending_status


ROME = ZoneInfo("Europe/Rome")


def at(value):
    return datetime.fromisoformat(value).replace(tzinfo=ROME)


@pytest.fixture
def jobs(profile, monkeypatch):
    from assistant import tick as runtime

    state = SimpleNamespace(age=timedelta(), error=None, code=0, sends=[], exports=[], snapshots=[])
    config = profile.settings.dwt_config
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text('{"digest_recipients": ["dev@example.com"]}')

    class Client:
        def __init__(self, p, *, now):
            assert p == profile
            self.now = now

        def __enter__(self):
            if state.error:
                raise state.error
            return self

        def __exit__(self, *exc):
            pass

        def call(self, tool, args=None):
            if tool == "health":
                return {"mailboxes": [{"last_sync_completed_at": (self.now - state.age).isoformat()}]}
            assert tool == "needs_reply"
            return {"items": [], "meta": {}}

    def send(command, **kwargs):
        assert command[1] == "send-note"
        state.sends.append(load_markers(profile))
        return subprocess.CompletedProcess(command, state.code)

    monkeypatch.setattr(runtime, "DwtClient", Client)
    monkeypatch.setattr(runtime, "export_new_sessions", lambda p: state.exports.append(p))
    monkeypatch.setattr(runtime, "take_snapshot", lambda p, now: state.snapshots.append(now))
    monkeypatch.setattr(runtime, "summarize_with_agent", lambda p: lambda turns: pytest.fail("paid call"))
    monkeypatch.setattr(runtime, "find_spec", lambda name: None)
    monkeypatch.setattr(subprocess, "run", send)
    state.runtime = runtime
    return state


def test_recap_due_first_tick_after_nine_when_asleep(profile, jobs):
    runtime = jobs.runtime
    first = at("2026-11-12T08:55")
    save_markers(profile, Markers(installed_at=first))
    assert "recap" not in runtime.tick(profile, first)
    assert load_markers(profile).installed_at == first
    assert "recap" in runtime.tick(profile, at("2026-11-12T11:40"))
    assert len(jobs.sends) == 1
    assert jobs.sends[0].recap_claim_start == first
    assert load_markers(profile).last_recap_day == date(2026, 11, 12)


def test_recap_due_once_per_day(profile, jobs):
    jobs.runtime.tick(profile, at("2026-11-12T09:00"))
    jobs.runtime.tick(profile, at("2026-11-12T09:15"))
    jobs.runtime.tick(profile, at("2026-11-12T23:59"))
    assert len(jobs.sends) == 1
    assert len(jobs.exports) == 4  # Daily recap refreshes exports before building.


def test_invalid_recipient_prevents_recap_export_and_paid_summary(profile, jobs, monkeypatch):
    profile.settings.dwt_config.write_text('{"digest_recipients": []}')
    monkeypatch.setattr(jobs.runtime, "DwtClient", lambda *a, **kw: pytest.fail("build before validation"))
    monkeypatch.setattr(jobs.runtime, "summarize_with_agent", lambda p: pytest.fail("paid work before validation"))
    save_markers(profile, Markers(installed_at=at("2026-11-11T09:00")))
    with pytest.raises(ValueError, match="recap_recipient"):
        jobs.runtime._recap(profile, at("2026-11-12T09:00"))
    assert jobs.exports == [] and jobs.sends == []
    assert load_markers(profile).recap_claim is None


def test_recap_weekend_gap_one_recap_monday(profile, jobs):
    friday = at("2026-11-13T09:00")
    save_markers(profile, Markers(installed_at=friday, last_recap_end=friday,
                                  last_recap_day=date(2026, 11, 13)))
    for day in ("2026-11-14T12:00", "2026-11-15T12:00"):
        assert "recap" not in jobs.runtime.tick(profile, at(day))
    jobs.runtime.tick(profile, at("2026-11-16T09:00"))
    jobs.runtime.tick(profile, at("2026-11-16T09:15"))
    assert len(jobs.sends) == 1
    assert jobs.sends[0].recap_claim_start == friday
    assert load_markers(profile).last_recap_end == at("2026-11-16T09:00")


def test_recap_long_absence_single_recap(profile, jobs):
    friday = at("2026-11-06T09:00")
    save_markers(profile, Markers(installed_at=friday, last_recap_end=friday,
                                  last_recap_day=date(2026, 11, 6)))
    jobs.runtime.tick(profile, at("2026-11-12T11:00"))
    assert len(jobs.sends) == 1
    assert jobs.sends[0].recap_claim_start == friday
    assert jobs.sends[0].recap_claim_end == at("2026-11-12T11:00")


def test_recap_deferred_once_when_mail_stale(profile, jobs):
    jobs.age = timedelta(minutes=16)
    first = at("2026-11-12T09:00")
    assert "recap" not in jobs.runtime.tick(profile, first)
    assert not jobs.sends
    assert load_markers(profile).recap_deferred_at == first
    assert "recap" in jobs.runtime.tick(profile, at("2026-11-12T09:15"))
    assert len(jobs.sends) == 1
    assert "Posta aggiornata alle 08:59." in (profile.memory_dir / "recap/latest.md").read_text()
    # Tomorrow gets its own deferral even after an overnight gap.
    assert "recap" not in jobs.runtime.tick(profile, at("2026-11-13T09:00"))
    assert len(jobs.sends) == 1


@pytest.mark.parametrize(("last", "now", "due"), [
    ("2026-11-11T18:00", "2026-11-12T17:59", False),
    ("2026-11-11T18:00", "2026-11-12T18:00", True),
    ("2026-11-11T19:00", "2026-11-12T18:00", False),
    ("2026-11-10T10:00", "2026-11-12T10:00", False),
    ("2026-11-10T10:00", "2026-11-12T10:00:01", True),
    (None, "2026-11-12T18:00", True),
])
def test_improve_due_after_18_or_overdue(last, now, due):
    from assistant.tick import due_jobs

    markers = Markers(installed_at=at("2026-11-11T10:00"),
                      last_improve_start=at(last) if last else None)
    assert ("improve" in due_jobs(markers, at(now), Settings(), ui_active=False,
                                 newest_snapshot_age=None)) == due


def test_first_improve_overdue_uses_install_marker():
    from assistant.tick import due_jobs

    markers = Markers(installed_at=at("2026-11-10T10:00"))
    assert "improve" not in due_jobs(markers, at("2026-11-12T10:00"), Settings(),
                                     ui_active=False, newest_snapshot_age=None)
    assert "improve" in due_jobs(markers, at("2026-11-12T10:00:01"), Settings(),
                                 ui_active=False, newest_snapshot_age=None)


def test_tick_lock_second_tick_does_nothing(profile, jobs):
    from assistant.locks import tick_lock

    def overlapping_export(p):
        assert jobs.runtime.tick(p, at("2026-11-12T11:01")) == []
        with tick_lock(p) as acquired:
            assert not acquired

    jobs.runtime.export_new_sessions = overlapping_export
    assert "recap" in jobs.runtime.tick(profile, at("2026-11-12T11:00"))
    assert len(jobs.sends) == 1


def test_failed_job_does_not_block_others(profile, jobs, monkeypatch, capsys):
    def fail(p):
        raise RuntimeError("export offline")

    monkeypatch.setattr(jobs.runtime, "export_new_sessions", fail)
    now = at("2026-11-12T11:00")
    assert jobs.runtime.tick(profile, now) == ["prune"]
    assert not jobs.sends
    assert load_markers(profile).job_fail_since == {"export": now, "recap": now}
    assert "RuntimeError: export offline" in capsys.readouterr().out


@pytest.mark.parametrize("recap_export_only", [False, True])
def test_recap_deferred_when_export_fails(profile, jobs, monkeypatch, recap_export_only):
    start = at("2026-11-11T09:00")
    now = at("2026-11-12T09:00")
    save_markers(profile, Markers(installed_at=start, last_recap_end=start,
                                  last_recap_day=start.date()))
    attempts = []
    failing = True

    def export(p):
        attempts.append(p)
        if failing and (not recap_export_only or len(attempts) == 2):
            raise RuntimeError("export offline")
        return []

    monkeypatch.setattr(jobs.runtime, "export_new_sessions", export)
    completed = jobs.runtime.tick(profile, now)
    assert "recap" not in completed
    assert "prune" in completed
    assert not jobs.sends
    markers = load_markers(profile)
    assert markers.last_recap_end == start and markers.last_recap_day == start.date()
    assert markers.recap_claim is None and markers.recap_deferred_at is None
    assert markers.job_fail_since["recap"] == now
    assert len(attempts) == 2
    failing = False
    assert "recap" in jobs.runtime.tick(profile, now + timedelta(minutes=15))
    assert len(jobs.sends) == 1
    assert jobs.sends[0].recap_claim_start == start
    assert "recap" not in load_markers(profile).job_fail_since


def test_improve_started_detached_not_awaited(profile, jobs, monkeypatch):
    started = []
    worker_lock = (profile.root / "improve.lock").open("a")

    def spawn(command, **kwargs):
        assert command == [sys.executable, "-m", "assistant", "bench", "improve-worker",
                           "--profile", "dev", "--state-root", str(profile.root.parents[1])]
        assert kwargs["start_new_session"] is True
        assert kwargs["stdout"].name == str(profile.logs_dir / "improve.log")
        assert kwargs["stderr"] is kwargs["stdout"]
        assert kwargs["stdin"] == subprocess.DEVNULL
        # Inspect the child priority callback without changing pytest's priority.
        kwargs["preexec_fn"]()
        fcntl.flock(worker_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        started.append(command)
        return SimpleNamespace(wait=lambda: pytest.fail("tick waited for worker"),
                               poll=lambda: pytest.fail("tick polled worker"))

    nice = []
    monkeypatch.setattr(jobs.runtime, "find_spec", lambda name: object())
    monkeypatch.setattr(subprocess, "Popen", spawn)
    monkeypatch.setattr(os, "nice", lambda value: nice.append(value))
    try:
        assert "improve" in jobs.runtime.tick(profile, at("2026-11-12T18:00"))
        assert "improve" not in jobs.runtime.tick(profile, at("2026-11-12T18:15"))
        assert len(started) == 1 and nice == [10]
        assert load_markers(profile).last_improve_start is None
    finally:
        worker_lock.close()


def test_job_failing_status_after_24h(profile, jobs, monkeypatch):
    def fail(p):
        raise RuntimeError("offline")

    monkeypatch.setattr(jobs.runtime, "export_new_sessions", fail)
    start = at("2026-11-14T11:00")  # Weekend keeps recap delivery out of this check.
    jobs.runtime.tick(profile, start)
    jobs.runtime.tick(profile, start + timedelta(hours=24))
    assert not pending_status(profile)
    jobs.runtime.tick(profile, start + timedelta(hours=24, seconds=1))
    assert [event.key for event in pending_status(profile)] == ["job_failing:export"]
    assert load_markers(profile).job_fail_since["export"] == start
    monkeypatch.setattr(jobs.runtime, "export_new_sessions", lambda p: [])
    jobs.runtime.tick(profile, start + timedelta(hours=24, minutes=15))
    assert not pending_status(profile)
    assert "export" not in load_markers(profile).job_fail_since


def test_pending_unknown_claim_closes_day_without_resend(profile, jobs):
    frozen = at("2026-11-12T09:00")
    save_markers(profile, Markers(installed_at=frozen - timedelta(days=1),
                                  recap_claim="recap:2026-11-12",
                                  recap_claim_start=frozen - timedelta(days=1), recap_claim_end=frozen))
    jobs.error = RuntimeError("recovery must not need dwt")
    jobs.runtime.tick(profile, at("2026-11-12T09:15"))
    markers = load_markers(profile)
    assert markers.last_recap_day == date(2026, 11, 12)
    assert markers.last_recap_end == frozen
    assert not jobs.sends
    assert markers.recap_claim == "recap:2026-11-12"
    assert [event.key for event in pending_status(profile)] == ["recap_unknown:2026-11-12"]


def test_old_claim_reconciled_by_its_date_then_today_sent(profile, jobs, monkeypatch):
    from assistant.locks import tick_lock

    start = at("2026-11-10T09:00")
    frozen = at("2026-11-11T09:00")
    now = at("2026-11-12T09:00")
    save_markers(profile, Markers(installed_at=start, last_recap_end=start,
                                  last_recap_day=start.date(), recap_claim="recap:2026-11-11",
                                  recap_claim_start=start, recap_claim_end=frozen))
    building = []
    build = jobs.runtime.build_recap

    def observe_build(*args, **kwargs):
        with tick_lock(profile) as acquired:
            assert not acquired
        building.append(load_markers(profile))
        return build(*args, **kwargs)

    monkeypatch.setattr(jobs.runtime, "build_recap", observe_build)
    assert "recap" in jobs.runtime.tick(profile, now)
    assert len(jobs.sends) == 1
    recovered, = building
    assert recovered.last_recap_day == date(2026, 11, 11)
    assert recovered.last_recap_end == frozen and recovered.recap_claim == "recap:2026-11-11"
    assert jobs.sends[0].recap_claim_start == frozen and jobs.sends[0].recap_claim_end == now
    text = (profile.memory_dir / "recap/2026-11-12.md").read_text()
    assert "Il recap del mercoledì 11 novembre potrebbe non essere arrivato: controlla la posta." in text
    markers = load_markers(profile)
    assert markers.last_recap_day == date(2026, 11, 12) and markers.last_recap_end == now
    assert markers.recap_claim == "recap:2026-11-12"
    assert pending_status(profile) == []
    jobs.runtime.tick(profile, now + timedelta(minutes=15))
    assert len(jobs.sends) == 1


@pytest.mark.parametrize("code", [0, 3, 4])
def test_delivery_outcome_preserves_markers(profile, jobs, code):
    jobs.code = code
    now = at("2026-11-12T11:00")
    installed = at("2026-11-12T08:00")
    save_markers(profile, Markers(installed_at=installed))
    jobs.runtime.tick(profile, now)
    markers = load_markers(profile)
    assert markers.installed_at == installed
    if code in (0, 3):
        assert markers.last_recap_end == now
        assert markers.last_recap_day == now.date()
    else:
        assert markers.last_recap_end is None and markers.last_recap_day is None
        assert markers.job_fail_since["recap"] == now
    jobs.runtime.tick(profile, now + timedelta(minutes=15))
    assert len(jobs.sends) == (2 if code == 4 else 1)


@pytest.mark.parametrize("has_birthtime", [False, True])
@pytest.mark.parametrize("has_recap", [False, True])
def test_tick_uses_settings_creation_for_older_install(profile, jobs, monkeypatch, has_birthtime, has_recap):
    installed = at("2026-11-10T08:00")
    previous = at("2026-11-11T09:00")
    now = at("2026-11-12T09:00")
    settings = profile.root / "settings.json"
    settings.write_text("{}")
    original_stat = Path.stat

    def stat(path, *args, **kwargs):
        result = original_stat(path, *args, **kwargs)
        if path != settings:
            return result
        times = {"st_mtime": (now if has_birthtime else installed).timestamp(), "st_mode": result.st_mode}
        if has_birthtime:
            times["st_birthtime"] = installed.timestamp()
        return SimpleNamespace(**times)

    monkeypatch.setattr(Path, "stat", stat)
    if has_recap:
        save_markers(profile, Markers(last_recap_end=previous, last_recap_day=previous.date()))
    assert "recap" in jobs.runtime.tick(profile, now)
    assert load_markers(profile).installed_at == installed
    assert jobs.sends[0].recap_claim_start == (previous if has_recap else installed)
    jobs.runtime.tick(profile, now + timedelta(minutes=15))
    assert load_markers(profile).installed_at == installed


@pytest.mark.parametrize("error", [TimeoutError("timeout"), RuntimeError("dwt offline")])
def test_mail_freshness_failure_is_retried(profile, jobs, error):
    jobs.error = error
    now = at("2026-11-12T11:00")
    jobs.runtime.tick(profile, now)
    assert load_markers(profile).job_fail_since["recap"] == now
    assert load_markers(profile).recap_deferred_at is None
    assert not jobs.sends
    jobs.error = None
    jobs.runtime.tick(profile, now + timedelta(minutes=15))
    assert len(jobs.sends) == 1
    assert "recap" not in load_markers(profile).job_fail_since


def test_markers_backward_compatible(profile):
    profile.markers_path.write_text('{"last_recap_day":"2026-11-11",'
                                    '"last_recap_end":"2026-11-11T09:00:00+01:00"}')
    markers = load_markers(profile)
    assert markers.installed_at is None
    assert markers.job_fail_since == {}
    assert markers.last_recap_end == at("2026-11-11T09:00")
    markers.installed_at = at("2026-11-10T08:00")
    markers.job_fail_since["recap"] = at("2026-11-12T09:00")
    save_markers(profile, markers)
    assert load_markers(profile) == markers
    assert json.loads(profile.markers_path.read_text())["job_fail_since"] == {
        "recap": "2026-11-12T08:00:00+00:00",
    }
    assert Markers().job_fail_since == {}


def test_tick_cli_lock_held_exit_0(profile, jobs, capsys):
    from assistant.cli import main
    from assistant.locks import tick_lock

    with tick_lock(profile) as acquired:
        assert acquired
        assert main(["tick", "--profile", "dev", "--state-root", str(profile.root.parents[1]),
                     "--now", "2026-11-12T11:00:00+01:00"]) == 0
    assert "tick già in corso" in capsys.readouterr().out
    assert not jobs.exports and not profile.markers_path.exists()


@pytest.mark.parametrize(("active", "age", "expected"), [
    (False, None, False), (False, timedelta(hours=2), False), (True, None, True),
    (True, timedelta(minutes=59, seconds=59), False), (True, timedelta(hours=1), True),
])
def test_snapshot_due_only_when_active_and_old(active, age, expected):
    from assistant.tick import due_jobs

    due = due_jobs(Markers(), at("2026-11-12T11:00"), Settings(), ui_active=active,
                   newest_snapshot_age=age)
    assert ("snapshot" in due) == expected


def test_tick_uses_snapshot_timestamp_not_mtime(profile, jobs):
    (profile.root / "ui.pid").write_text(str(os.getpid()))
    profile.snapshots_dir.mkdir()
    # A just-created file containing an hour-old snapshot is already due.
    (profile.snapshots_dir / "dwt-20261112T090000Z.db").touch()
    jobs.runtime.tick(profile, at("2026-11-12T10:59:59"))
    assert not jobs.snapshots
    jobs.runtime.tick(profile, at("2026-11-12T11:00:00"))
    assert jobs.snapshots == [at("2026-11-12T11:00:00")]


def test_prune_daily_in_rome_and_preserves_exports_pins_and_symlinks(profile, jobs, tmp_path):
    now = at("2026-11-14T00:05")
    root = profile.root / "ops-workspaces"
    root.mkdir()
    for name, age in (("old", 86401), ("boundary", 86400), ("new", 30)):
        path = root / name
        path.mkdir()
        os.utime(path, (now.timestamp() - age, now.timestamp() - age))
    external = tmp_path / "external"
    external.mkdir()
    (root / "link").symlink_to(external, target_is_directory=True)
    profile.exports_dir.mkdir()
    (profile.exports_dir / "keep.json").write_text("export")
    profile.snapshots_dir.mkdir()
    for name in ("dwt-20260101T000000Z.db", "dwt-20260102T000000Z.db"):
        (profile.snapshots_dir / name).touch()
    profile.bench_dir.mkdir()
    (profile.bench_dir / "pins.json").write_text('{"snapshots":["dwt-20260101T000000Z.db"]}')
    assert "prune" in jobs.runtime.tick(profile, now)
    assert sorted(path.name for path in root.iterdir()) == ["boundary", "link", "new"]
    assert external.is_dir() and (profile.exports_dir / "keep.json").read_text() == "export"
    assert [path.name for path in profile.snapshots_dir.iterdir()] == ["dwt-20260101T000000Z.db"]
    assert "prune" not in jobs.runtime.tick(profile, now + timedelta(hours=23))
    assert (root / "boundary").is_dir()
    assert "prune" in jobs.runtime.tick(profile, now + timedelta(days=1))
    assert not (root / "boundary").exists()


def test_due_jobs_order_and_rome_day():
    from assistant.tick import due_jobs

    markers = Markers(last_prune=at("2026-11-11T23:30"))
    now = at("2026-11-12T18:00")
    assert due_jobs(markers, now, Settings(), ui_active=True, newest_snapshot_age=None) == [
        "export", "snapshot", "recap", "improve", "prune",
    ]
    # UTC Wednesday 23:30 is already Thursday in Rome, before recap time.
    markers.last_prune = datetime(2026, 11, 11, 23, 0, tzinfo=timezone.utc)
    assert due_jobs(markers, datetime(2026, 11, 11, 23, 30, tzinfo=timezone.utc), Settings(),
                    ui_active=False, newest_snapshot_age=None) == ["export"]
