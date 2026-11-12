"""Atomic progress markers for recap, improvement and pruning jobs."""

from dataclasses import asdict, dataclass, field, fields
from datetime import date, datetime, timezone
import json
import os

from assistant.install import _atomic_write
from assistant.notes_mcp import _require_aware
from assistant.profile import Profile, _read_object


@dataclass
class Markers:
    last_recap_end: datetime | None = None
    last_recap_day: date | None = None
    recap_claim: str | None = None
    recap_deferred_at: datetime | None = None
    last_improve_start: datetime | None = None
    improve_run: str | None = None
    last_prune: datetime | None = None
    recap_claim_start: datetime | None = None
    recap_claim_end: datetime | None = None
    installed_at: datetime | None = None
    job_fail_since: dict[str, datetime] = field(default_factory=dict)


_TIMES = ("last_recap_end", "recap_deferred_at", "last_improve_start", "last_prune",
          "recap_claim_start", "recap_claim_end", "installed_at")


def load_markers(p: Profile) -> Markers:
    values = _read_object(p.markers_path)
    values = {field.name: values[field.name] for field in fields(Markers) if field.name in values}
    for key in _TIMES:
        if values.get(key) is not None:
            at = datetime.fromisoformat(values[key])
            _require_aware(at)
            values[key] = at.astimezone(timezone.utc)
    if values.get("last_recap_day") is not None:
        values["last_recap_day"] = date.fromisoformat(values["last_recap_day"])
    failures = {}
    for job, value in values.get("job_fail_since", {}).items():
        at = datetime.fromisoformat(value)
        _require_aware(at)
        failures[job] = at.astimezone(timezone.utc)
    values["job_fail_since"] = failures
    return Markers(**values)


def save_markers(p: Profile, m: Markers) -> None:
    values = asdict(m)
    for key in _TIMES:
        at = values[key]
        if at is not None:
            _require_aware(at)
            values[key] = at.astimezone(timezone.utc).isoformat()
    if m.last_recap_day is not None:
        values["last_recap_day"] = m.last_recap_day.isoformat()
    for job, at in m.job_fail_since.items():
        _require_aware(at)
        values["job_fail_since"][job] = at.astimezone(timezone.utc).isoformat()
    _atomic_write(p.markers_path, json.dumps(values, ensure_ascii=False, indent=2) + "\n")
    # The file is fsynced before rename; also persist the new directory entry
    # before a claimed send can start, so a crash cannot undo its receipt.
    directory = os.open(p.markers_path.parent, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)
