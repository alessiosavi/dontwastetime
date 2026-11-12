"""Pending Italian status messages, retained until a delivered recap acknowledges them."""

from dataclasses import dataclass
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import json

from assistant.install import _atomic_write
from assistant.notes_mcp import _require_aware
from assistant.profile import Profile


@dataclass(frozen=True)
class StatusEvent:
    key: str
    text: str
    at: datetime


def pending_status(p: Profile) -> list[StatusEvent]:
    try:
        rows = json.loads((p.root / "status.json").read_text(encoding="utf-8"))
    except FileNotFoundError:
        return []
    events = []
    for row in rows:
        at = datetime.fromisoformat(row["at"])
        _require_aware(at)
        events.append(StatusEvent(row["key"], row["text"], at.astimezone(timezone.utc)))
    return sorted(events, key=lambda event: event.at, reverse=True)


def _save(p: Profile, events: list[StatusEvent]) -> None:
    rows = [{"key": e.key, "text": e.text, "at": e.at.isoformat()} for e in events]
    _atomic_write(p.root / "status.json", json.dumps(rows, ensure_ascii=False, indent=2) + "\n")
    _render_status(p)


@contextmanager
def _status_lock(p: Profile):
    p.root.mkdir(mode=0o700, parents=True, exist_ok=True)
    # status.json is atomically replaced, so lock a separate stable inode.
    with (p.root / "status.lock").open("a", encoding="utf-8") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        yield


def record_status(p: Profile, key: str, text: str, now: datetime) -> None:
    _require_aware(now)
    with _status_lock(p):
        events = pending_status(p)
        if not any(event.key == key for event in events):
            events.append(StatusEvent(key, text, now.astimezone(timezone.utc)))
            _save(p, events)
        else:
            _render_status(p)


def ack_status(p: Profile, keys: list[str]) -> None:
    with _status_lock(p):
        _save(p, [event for event in pending_status(p) if event.key not in keys])


def render_status(p: Profile) -> None:
    with _status_lock(p):
        _render_status(p)


def _render_status(p: Profile) -> None:
    text = "".join(f"- {event.text}\n" for event in pending_status(p))
    _atomic_write(p.memory_dir / "status.md", text[:1000])
