"""Consistent, timestamped dwt copies; the only direct runtime DB access."""

from contextlib import closing
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import sqlite3
import tempfile

from assistant.profile import Profile
from assistant.notes_mcp import _require_aware


def _snapshots(p: Profile) -> list[tuple[datetime, Path]]:
    result = []
    for path in p.snapshots_dir.glob("dwt-*.db"):
        try:
            at = datetime.strptime(path.name, "dwt-%Y%m%dT%H%M%SZ.db").replace(tzinfo=timezone.utc)
        except ValueError:
            continue
        if path.is_file() and not path.is_symlink():
            result.append((at, path))
    return sorted(result)


def snapshot_before(p: Profile, at: datetime) -> Path | None:
    _require_aware(at)
    return next((path for taken, path in reversed(_snapshots(p)) if taken <= at), None)


def take_snapshot(p: Profile, now: datetime) -> Path:
    _require_aware(now)
    for taken, path in reversed(_snapshots(p)):
        if timedelta(0) <= now - taken < timedelta(minutes=60):
            return path
    p.snapshots_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    name = f"dwt-{now.astimezone(timezone.utc):%Y%m%dT%H%M%SZ}.db"
    target = p.snapshots_dir / name
    fd, name = tempfile.mkstemp(prefix=".dwt-", suffix=".tmp", dir=p.snapshots_dir)
    os.close(fd)
    temporary = Path(name)
    try:
        source_uri = (p.dwt_state_dir / "dwt.db").resolve().as_uri() + "?mode=ro"
        with closing(sqlite3.connect(source_uri, uri=True)) as source:
            with closing(sqlite3.connect(temporary)) as dest:
                source.backup(dest)
        with temporary.open("rb") as backup:
            os.fsync(backup.fileno())
        os.replace(temporary, target)
        return target
    finally:
        temporary.unlink(missing_ok=True)


def prune_snapshots(p: Profile, now: datetime) -> int:
    _require_aware(now)
    try:
        pins = json.loads((p.bench_dir / "pins.json").read_text(encoding="utf-8"))["snapshots"]
    except FileNotFoundError:
        pins = []
    pinned = {str(pin) for pin in pins}
    removed = 0
    for taken, path in _snapshots(p):
        names = {path.name, str(path), str(path.relative_to(p.root))}
        if now - taken > timedelta(days=56) and not names.intersection(pinned):
            path.unlink()
            removed += 1
    return removed


def ui_active(p: Profile) -> bool:
    try:
        pid = int((p.root / "ui.pid").read_text(encoding="utf-8").strip())
        if pid <= 0:
            return False
        os.kill(pid, 0)
    except PermissionError:
        return True  # The process exists, although we cannot signal it.
    except (FileNotFoundError, ProcessLookupError, ValueError, OverflowError):
        return False
    return True
