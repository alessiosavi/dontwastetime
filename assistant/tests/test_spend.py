"""Accounting counts per-message timestamps and never treats unknown prices as free."""

from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from datetime import datetime, timezone
import json
from pathlib import Path
from zoneinfo import ZoneInfo

import pytest


ROME = ZoneInfo("Europe/Rome")
NOW = datetime(2026, 11, 12, 10, tzinfo=timezone.utc)
FIXTURE = Path(__file__).parent / "fixtures/session_export.json"


def test_record_usage_idempotent(profile):
    from assistant.spend import record_usage, spend_between

    def record(_):
        record_usage(profile, "call-1", NOW, profile.settings.model, 10, 20, 2.5)

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(record, range(8)))
    lines = (profile.root / "usage.jsonl").read_text().splitlines()
    assert len(lines) == 1
    row = json.loads(lines[0])
    assert row["call_id"] == "call-1" and row["tokens_cache_read"] == 0
    assert row["at"] == "2026-11-12T10:00:00+00:00"
    total = spend_between(profile, NOW, datetime(2026, 11, 13, tzinfo=timezone.utc))
    assert total.usd == 2.5 and total.unknown_tokens == 0
    assert spend_between(profile, NOW, NOW).usd == 0


def test_usage_price_fallback(profile):
    from assistant.spend import record_usage, spend_between

    p = replace(profile, settings=replace(profile.settings,
                model_cost={"input": 2, "output": 6, "cache_read": 0.5}))
    record_usage(p, "fallback", NOW, p.settings.model, 1_000_000, 500_000, 0,
                 tokens_cache_read=2_000_000)
    record_usage(p, "actual", NOW, p.settings.model, 1_000_000, 500_000, 7)
    record_usage(p, "other-model", NOW, "other/model", 100, 200, None)
    total = spend_between(p, NOW, datetime(2026, 11, 13, tzinfo=timezone.utc))
    assert total.usd == 13.0
    assert total.unknown_tokens == 300


def test_usage_split_across_month_boundary(profile):
    from assistant.spend import usage_from_export, spend_between

    data = json.loads(FIXTURE.read_text())
    assistants = [m for m in data["messages"] if m["type"] == "assistant"]
    times = [datetime(2026, 10, 31, 23, 50, tzinfo=ROME),
             datetime(2026, 11, 1, 0, 10, tzinfo=ROME),
             datetime(2026, 11, 1, 0, 20, tzinfo=ROME)]
    for message, at, cost in zip(assistants, times, [2, 3, 4], strict=True):
        message["time"]["created"] = int(at.timestamp() * 1000)
        message["cost"] = cost
    profile.exports_dir.mkdir()
    path = profile.exports_dir / "session.json"
    path.write_text(json.dumps(data))
    (profile.exports_dir / "index.json").write_text('{"session":123}')
    lines = usage_from_export(path)
    assert len(lines) == 3
    assert [line.at for line in lines] == times
    start = datetime(2026, 11, 1, tzinfo=ROME)
    assert spend_between(profile, start, NOW).usd == 7
    assert spend_between(profile, times[0], start).usd == 2


def test_export_usage_fallback_includes_cache_tokens(profile):
    from assistant.spend import usage_from_export, spend_between

    p = replace(profile, settings=replace(profile.settings,
                model_cost={"input": 2, "output": 6, "cache_read": 0.5}))
    p.exports_dir.mkdir()
    path = p.exports_dir / "fixture.json"
    path.write_bytes(FIXTURE.read_bytes())
    lines = usage_from_export(path)
    assert sum(line.tokens_in for line in lines) == 1593
    assert sum(line.tokens_out for line in lines) == 85
    assert sum(line.tokens_cache_read for line in lines) == 3072
    start, end = datetime(2026, 11, 1, tzinfo=ROME), datetime(2026, 12, 1, tzinfo=ROME)
    assert spend_between(p, start, end).usd == pytest.approx(0.005232)


def test_spend_warns_above_soft_cap(profile):
    from assistant.spend import record_usage, spend_lines

    record_usage(profile, "nov", datetime(2026, 11, 1, 0, 10, tzinfo=ROME),
                 profile.settings.model, 10, 10, 101)
    record_usage(profile, "oct", datetime(2026, 10, 31, 23, 50, tzinfo=ROME),
                 profile.settings.model, 10, 10, 50)
    lines = spend_lines(profile, datetime(2026, 11, 10, tzinfo=ROME), NOW)
    assert lines == ["Costo da 10/11/2026: $0.00", "Questo mese: $101.00",
                     "⚠ sopra la soglia di $100"]


def test_spend_unknown_cost_line(profile):
    from assistant.spend import record_usage, spend_lines

    record_usage(profile, "unknown", NOW.replace(hour=9), profile.settings.model, 10, 20, 0,
                 tokens_cache_read=30)
    assert "Costo non disponibile per 60 token (prezzo del modello non configurato)" in (
        spend_lines(profile, NOW.replace(hour=8), NOW)
    )


def test_spend_rejects_naive_datetimes(profile):
    from assistant.spend import record_usage, spend_between, spend_lines

    naive = NOW.replace(tzinfo=None)
    with pytest.raises(ValueError):
        record_usage(profile, "bad", naive, "model", 1, 1, 0)
    for function in (spend_between, spend_lines):
        with pytest.raises(ValueError):
            function(profile, naive, NOW)
