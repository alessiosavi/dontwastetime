"""A10 accounting: our idempotent usage ledger plus per-message UI exports."""

from dataclasses import asdict, dataclass, replace
from datetime import datetime, timezone
import fcntl
import json
import os
from pathlib import Path
from zoneinfo import ZoneInfo

from assistant.notes_mcp import _require_aware
from assistant.profile import Profile


ROME = ZoneInfo("Europe/Rome")


@dataclass(frozen=True)
class UsageLine:
    call_id: str
    at: datetime
    model: str
    tokens_in: int
    tokens_out: int
    cost_usd: float | None
    tokens_cache_read: int = 0


@dataclass(frozen=True)
class SpendTotal:
    usd: float = 0.0
    unknown_tokens: int = 0


def _cost(p: Profile, line: UsageLine) -> float | None:
    if line.cost_usd is not None and line.cost_usd > 0:
        return line.cost_usd
    rates = p.settings.model_cost
    if rates is None or line.model.split("#", 1)[0] != p.settings.model.split("#", 1)[0]:
        return None
    return (line.tokens_in * rates["input"] + line.tokens_out * rates["output"]
            + line.tokens_cache_read * rates["cache_read"]) / 1_000_000


def _usage_records(p: Profile) -> list[UsageLine]:
    try:
        text = (p.root / "usage.jsonl").read_text(encoding="utf-8")
    except FileNotFoundError:
        return []
    records = []
    seen = set()
    for line in text.splitlines():
        row = json.loads(line)
        if row["call_id"] in seen:
            continue
        seen.add(row["call_id"])
        row["at"] = datetime.fromisoformat(row["at"])
        _require_aware(row["at"])
        records.append(UsageLine(**row))
    return records


def record_usage(
    p: Profile, call_id: str, at: datetime, model: str, tokens_in: int, tokens_out: int,
    cost_usd: float | None, *, tokens_cache_read: int = 0,
) -> None:
    _require_aware(at)
    line = UsageLine(call_id, at.astimezone(timezone.utc), model, tokens_in, tokens_out,
                     cost_usd, tokens_cache_read)
    line = replace(line, cost_usd=_cost(p, line))
    p.root.mkdir(mode=0o700, parents=True, exist_ok=True)
    # A stable lock inode serializes the check-and-append across processes too.
    with (p.root / "usage.lock").open("a", encoding="utf-8") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        if any(previous.call_id == call_id for previous in _usage_records(p)):
            return
        row = asdict(line)
        row["at"] = line.at.isoformat()
        with (p.root / "usage.jsonl").open("a", encoding="utf-8") as output:
            output.write(json.dumps(row, ensure_ascii=False) + "\n")
            output.flush()
            os.fsync(output.fileno())


def usage_from_export(path: Path) -> list[UsageLine]:
    data = json.loads(path.read_text(encoding="utf-8"))
    lines = []
    for message in data["messages"]:
        if message["type"] != "assistant":
            continue
        model = message.get("model") or data["info"].get("model", {})
        name = f"{model['providerID']}/{model['id']}"
        tokens = message.get("tokens", {})
        lines.append(UsageLine(
            f"{data['info']['id']}:{message['id']}",
            datetime.fromtimestamp(message["time"]["created"] / 1000, timezone.utc),
            name, tokens.get("input", 0), tokens.get("output", 0), message.get("cost"),
            tokens.get("cache", {}).get("read", 0),
        ))
    return lines


def spend_between(p: Profile, start: datetime, end: datetime) -> SpendTotal:
    _require_aware(start)
    _require_aware(end)
    lines = _usage_records(p)
    for path in sorted(p.exports_dir.glob("*.json")):
        if path.name != "index.json":
            lines.extend(usage_from_export(path))
    usd, unknown = 0.0, 0
    for line in lines:
        if start <= line.at < end:
            cost = _cost(p, line)
            if cost is None:
                unknown += line.tokens_in + line.tokens_out + line.tokens_cache_read
            else:
                usd += cost
    return SpendTotal(usd, unknown)


def spend_lines(p: Profile, period_start: datetime, now: datetime) -> list[str]:
    _require_aware(period_start)
    _require_aware(now)
    month_start = now.astimezone(ROME).replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    period = spend_between(p, period_start, now)
    month = spend_between(p, month_start, now)
    lines = [f"Costo da {period_start.astimezone(ROME):%d/%m/%Y}: ${period.usd:.2f}",
             f"Questo mese: ${month.usd:.2f}"]
    if month.usd > p.settings.soft_cap_usd:
        lines.append(f"⚠ sopra la soglia di ${p.settings.soft_cap_usd:g}")
    # Both windows end now: their union starts at the earlier boundary.
    unknown = spend_between(p, min(period_start, month_start), now).unknown_tokens
    if unknown:
        lines.append(f"Costo non disponibile per {unknown} token (prezzo del modello non configurato)")
    return lines
