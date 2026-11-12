"""Status stays pending until acknowledged and always respects the prompt cap."""

from datetime import datetime, timedelta, timezone
from concurrent.futures import ThreadPoolExecutor
import time

import pytest


def test_status_dedup_and_cap(profile):
    from assistant.status import record_status, pending_status, ack_status, render_status

    now = datetime(2026, 11, 12, 10, tzinfo=timezone.utc)
    assert pending_status(profile) == []
    for index in range(15):
        record_status(profile, f"job_failing:{index}", f"Errore {index}: " + "x" * 90,
                      now + timedelta(seconds=index))
    record_status(profile, "job_failing:14", "Duplicato", now + timedelta(hours=1))
    assert len(pending_status(profile)) == 15
    render_status(profile)
    text = (profile.memory_dir / "status.md").read_text()
    assert len(text) <= 1000
    assert "Errore 14" in text and text.index("Errore 14") < text.index("Errore 13")
    assert "Duplicato" not in text
    ack_status(profile, [f"job_failing:{i}" for i in range(15)])
    assert pending_status(profile) == []
    assert (profile.memory_dir / "status.md").read_text() == ""


def test_status_survives_reload_and_caps_one_long_event(profile):
    from assistant.status import record_status, pending_status, render_status

    now = datetime(2026, 11, 12, 10, tzinfo=timezone.utc)
    record_status(profile, "promoted:1", "Versione migliorata: " + "è" * 1200, now)
    event, = pending_status(profile)
    assert event.key == "promoted:1" and event.at == now
    render_status(profile)
    assert len((profile.memory_dir / "status.md").read_text()) <= 1000
    with pytest.raises(ValueError):
        record_status(profile, "x", "Data senza fuso", now.replace(tzinfo=None))


def test_concurrent_status_writes_are_safe(profile, monkeypatch):
    from assistant import status

    original = status._save

    def slow_save(p, events):
        time.sleep(0.005)  # Widen the read/modify/write race deterministically.
        original(p, events)

    monkeypatch.setattr(status, "_save", slow_save)
    now = datetime(2026, 11, 12, 10, tzinfo=timezone.utc)
    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(lambda i: status.record_status(profile, f"k{i % 24}", f"Evento {i % 24}", now), range(48)))
    assert {event.key for event in status.pending_status(profile)} == {f"k{i}" for i in range(24)}
    assert len(status.pending_status(profile)) == 24
    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(lambda i: status.ack_status(profile, [f"k{i}"]), range(24)))
    assert status.pending_status(profile) == []
    assert (profile.memory_dir / "status.md").read_text() == ""
