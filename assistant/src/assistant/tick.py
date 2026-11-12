"""One due-based pass over the profile's jobs, serialized with manual recaps."""

from datetime import datetime, timedelta, timezone
import fcntl
from importlib.util import find_spec
import os
import shutil
import subprocess
import sys
import traceback
from zoneinfo import ZoneInfo

from assistant.dwt_client import DwtClient
from assistant.install import installation_time
from assistant.locks import tick_lock
from assistant.markers import Markers, load_markers, save_markers
from assistant.notes_mcp import _require_aware
from assistant.profile import Profile, Settings
from assistant.recap import (
    build_recap, deliver_recap, mail_freshness, recap_recipient, reconcile_claim, summarize_with_agent,
)
from assistant.sessions import export_new_sessions
from assistant.snapshot import _snapshots, prune_snapshots, take_snapshot, ui_active
from assistant.status import ack_status, record_status


ROME = ZoneInfo("Europe/Rome")


def due_jobs(
    m: Markers, now: datetime, s: Settings, *,
    ui_active: bool, newest_snapshot_age: timedelta | None,
) -> list[str]:
    """Decide from wall time and durable boundaries, never the previous tick."""
    _require_aware(now)
    local = now.astimezone(ROME)
    now = now.astimezone(timezone.utc)
    jobs = ["export"]
    if ui_active and (newest_snapshot_age is None or newest_snapshot_age >= timedelta(hours=1)):
        jobs.append("snapshot")
    if local.weekday() < 5 and local.hour >= 9 and m.last_recap_day != local.date():
        jobs.append("recap")
    interval = timedelta(hours=s.improve_every_hours)
    baseline = m.last_improve_start or m.installed_at or now
    if ((m.last_improve_start is None or now - m.last_improve_start >= interval)
            and (local.hour >= 18 or now - baseline > interval + timedelta(hours=24))):
        jobs.append("improve")
    if m.last_prune is None or m.last_prune.astimezone(ROME).date() != local.date():
        jobs.append("prune")
    return jobs


def _recap(p: Profile, now: datetime) -> bool:
    reconcile_claim(p, now)
    m = load_markers(p)
    today = now.astimezone(ROME).date()
    if m.recap_claim == f"recap:{today.isoformat()}":
        return True
    recap_recipient(p)
    with DwtClient(p, now=now) as client:
        if (mail_freshness(client, now) > timedelta(minutes=15)
                and (m.recap_deferred_at is None or m.recap_deferred_at.astimezone(ROME).date() != today)):
            m.recap_deferred_at = now
            save_markers(p, m)
            return False
        start = m.last_recap_end or m.installed_at
        assert start is not None  # Initialized by tick before dispatching jobs.
        export_new_sessions(p)
        recap = build_recap(p, start, now, client=client, summarize=summarize_with_agent(p))
    outcome = deliver_recap(p, recap, now)
    if outcome == "failed":
        raise RuntimeError("Il recap non è stato inviato; riproverò al psamplemo tick.")
    # Delivery writes claims and may acknowledge pending status. Never overwrite
    # those writes using the pre-send copy of the markers.
    m = load_markers(p)
    end = m.recap_claim_end if outcome == "skipped" else recap.period_end
    if end is None:
        raise RuntimeError("Il recap già preso in carico non ha una fine del periodo registrata.")
    m.last_recap_day, m.last_recap_end = today, end
    save_markers(p, m)
    return True


def _background_priority() -> None:
    os.nice(10)


def _improve(p: Profile) -> bool:
    if find_spec("assistant.bench") is None:
        return False
    with (p.root / "improve.lock").open("a", encoding="utf-8") as lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return False
        fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
    p.logs_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (p.logs_dir / "improve.log").open("a", encoding="utf-8") as log:
        subprocess.Popen(
            [sys.executable, "-m", "assistant", "bench", "improve-worker",
             "--profile", p.name, "--state-root", str(p.root.parent.parent)],
            cwd=p.root, start_new_session=True, stdin=subprocess.DEVNULL,
            stdout=log, stderr=log, preexec_fn=_background_priority,
        )
    # The worker owns last_improve_start and its own lifetime, including resume.
    return True


def _prune(p: Profile, now: datetime) -> None:
    prune_snapshots(p, now)
    workspaces = p.root / "ops-workspaces"
    if workspaces.is_dir():
        for path in workspaces.iterdir():
            if (not path.is_symlink() and path.is_dir()
                    and now.timestamp() - path.stat().st_mtime > 86_400):
                shutil.rmtree(path)
    # Exports remain intact until bench pinning defines their retention contract.
    m = load_markers(p)
    m.last_prune = now
    save_markers(p, m)


def _failed(p: Profile, job: str, now: datetime, error: Exception) -> None:
    print(f"Assistente: errore nel job {job}.", flush=True)
    traceback.print_exception(type(error), error, error.__traceback__, file=sys.stdout)
    m = load_markers(p)
    since = m.job_fail_since.setdefault(job, now)
    save_markers(p, m)
    if now - since > timedelta(hours=24):
        name = {"export": "L'esportazione delle conversazioni", "snapshot": "La copia dello stato",
                "recap": "Il recap", "improve": "Il miglioramento dell'assistente",
                "prune": "La pulizia dei file"}[job]
        record_status(p, f"job_failing:{job}",
                      f"{name} non riesce a partire da ieri: avvisa chi ti ha installato l'assistente.", now)


def _succeeded(p: Profile, job: str) -> None:
    m = load_markers(p)
    if job in m.job_fail_since:
        del m.job_fail_since[job]
        save_markers(p, m)
    ack_status(p, [f"job_failing:{job}"])


def tick(p: Profile, now: datetime) -> list[str]:
    """Return completed/launched jobs; deferrals, held locks and failures aren't successes."""
    _require_aware(now)
    now = now.astimezone(timezone.utc)
    with tick_lock(p) as acquired:
        if not acquired:
            print("tick già in corso", flush=True)
            return []
        m = load_markers(p)
        if m.installed_at is None:
            m.installed_at = installation_time(p, now)
            save_markers(p, m)
        active, age = False, None
        try:
            active = ui_active(p)
            if active:
                taken = next((at for at, _ in reversed(_snapshots(p)) if at <= now), None)
                age = now - taken if taken is not None else None
        except Exception as error:
            _failed(p, "snapshot", now, error)
            active = False
        completed = []
        for job in due_jobs(m, now, p.settings, ui_active=active, newest_snapshot_age=age):
            try:
                if job == "export":
                    export_new_sessions(p)
                elif job == "snapshot":
                    take_snapshot(p, now)
                elif job == "recap":
                    if not _recap(p, now):
                        continue
                elif job == "improve":
                    if not _improve(p):
                        continue
                elif job == "prune":
                    _prune(p, now)
                _succeeded(p, job)
                completed.append(job)
            except Exception as error:
                _failed(p, job, now, error)
        return completed
