"""Durable markers retain timezone information and survive interrupted writes."""

from datetime import date, datetime, timezone
import json

import pytest


def test_markers_roundtrip_and_atomic_write(profile, monkeypatch):
    from assistant.markers import Markers, load_markers, save_markers
    import os

    assert load_markers(profile) == Markers()
    now = datetime(2026, 11, 12, 10, tzinfo=timezone.utc)
    markers = Markers(now, date(2026, 11, 12), "recap:today", now, now, "run-1", now)
    save_markers(profile, markers)
    assert load_markers(profile) == markers
    saved = profile.markers_path.read_bytes()
    assert json.loads(saved)["last_recap_day"] == "2026-11-12"

    def interrupted(*args):
        raise OSError("interrupted rename")

    monkeypatch.setattr(os, "replace", interrupted)
    with pytest.raises(OSError, match="interrupted"):
        save_markers(profile, Markers())
    assert profile.markers_path.read_bytes() == saved
    assert not list(profile.root.glob(".markers.json*"))


def test_markers_reject_naive_datetimes(profile):
    from assistant.markers import Markers, load_markers, save_markers

    with pytest.raises(ValueError):
        save_markers(profile, Markers(last_recap_end=datetime(2026, 11, 12)))
    profile.markers_path.write_text('{"last_prune":"2026-11-12T00:00:00"}')
    with pytest.raises(ValueError):
        load_markers(profile)
