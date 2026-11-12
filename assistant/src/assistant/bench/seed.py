"""Resolve private seed turns at runtime, never into the source tree.

Placeholder grammar (no eval, recursive interpolation or write tools):
  {{tool[:args].index.field[.field...]}}; index is zero-based.
  tool is a dwt read tool; args is a flat JSON object, or literal search words.
  Rows are result.items, result.messages, or [result] for a singleton result.
  Fields use exact JSON keys; bare mail text fields also look under untrusted.
  Missing rows/fields, invalid syntax and dwt errors skip the turn with a warning.

Examples: {{needs_reply.0.inbound_id}}, {{search:Sample fattura.0.subject}},
{{needs_reply:{"state":"due"}.0.thread}}, {{context.0.today}}.

build_seed_turns returns Turn-compatible SeededTurn objects. Their .seed holds
the *resolved* expectations, stub and split for checks and subsequent rollouts.
The same metadata is persisted beside each turn in bench/seed/turns.jsonl.
"""

from contextlib import ExitStack, closing
from dataclasses import asdict, dataclass, replace
from datetime import datetime, timezone
import json
import logging
from pathlib import Path
import re
import sqlite3
import tempfile
from typing import Literal

from assistant.dwt_client import DwtClient
from assistant.notes_mcp import _atomic_write, _require_aware
from assistant.profile import Profile
from assistant.sessions import Turn


DATA_DIR = Path(__file__).resolve().parents[3] / "bench_data"
if not DATA_DIR.is_dir():  # Non-editable installs carry assets inside the package.
    DATA_DIR = Path(__file__).resolve().parents[1] / "bench_data"
READ_TOOLS = ("context", "overview", "needs_reply", "list_messages", "search",
              "get_thread", "get_message", "digest", "health")
logger = logging.getLogger(__name__)
PLACEHOLDER_RE = re.compile(r"\{\{([^{}]*(?:\{[^{}]*\}[^{}]*)?)\}\}")
EXPR_RE = re.compile(r"([a-z_]+)(?::(.*))?\.(\d+)\.([a-z_]+(?:\.[a-z_]+)*)", re.S)


@dataclass(frozen=True)
class SeedTurn:
    id: str
    message_template: str
    history: list[tuple[str, str]]
    at: str
    expect_ops: list[dict]
    forbid_tools: list[str]
    stub: str | None
    split: Literal["train", "holdout"]
    appendix_row: str = ""


@dataclass
class SeededTurn(Turn):
    seed: SeedTurn | None = None


def load_seed_turns(path: Path | None = None) -> list[SeedTurn]:
    records = []
    for line in (path or DATA_DIR / "seed_turns.jsonl").read_text(encoding="utf-8").splitlines():
        if line.strip():
            item = json.loads(line)
            item["history"] = [tuple(pair) for pair in item["history"]]
            records.append(SeedTurn(**item))
    return records


class PlaceholderError(ValueError):
    """This template is not resolvable on this particular dataset."""


class _Resolver:
    def __init__(self, client):
        self.client = client
        self.cache = {}

    def text(self, template: str) -> str:
        def resolve(match):
            expression = EXPR_RE.fullmatch(match[1])
            if expression is None or expression[1] not in READ_TOOLS:
                raise PlaceholderError("segnaposto non valido o strumento non di lettura")
            tool, arguments, index, field = expression.groups()
            args = {}
            if arguments is not None:
                if arguments.startswith("{"):
                    args = json.loads(arguments)
                    if not isinstance(args, dict):
                        raise PlaceholderError("gli argomenti devono essere un oggetto JSON")
                elif tool == "search" and arguments.strip():
                    args = {"query": arguments}
                else:
                    raise PlaceholderError("usa argomenti JSON oppure parole per search")
            key = (tool, json.dumps(args, sort_keys=True))
            if key not in self.cache:
                self.cache[key] = self.client.call(tool, args)
            result = self.cache[key]
            rows = result.get("items", result.get("messages", [result]))
            try:
                value = rows[int(index)]
                for part in field.split("."):
                    if isinstance(value, dict) and part not in value:
                        value = value.get("untrusted", {})
                    value = value[part]
            except (KeyError, IndexError, TypeError):
                # Do not log source text, subjects or argument values.
                raise PlaceholderError(f"{tool}: elemento {index} o campo {field} assente") from None
            if value is None or not isinstance(value, (str, int, float, bool)):
                raise PlaceholderError(f"{tool}: il campo {field} non è uno scalare")
            return str(value)

        text = PLACEHOLDER_RE.sub(resolve, template)
        if "{{" in text or "}}" in text:
            raise PlaceholderError("segnaposto incompleto o annidato")
        return text


def _snapshot_time(snapshot: Path) -> datetime:
    try:
        return datetime.strptime(snapshot.name, "dwt-%Y%m%dT%H%M%SZ.db").replace(tzinfo=timezone.utc)
    except ValueError:
        # Canonical filenames carry creation time; imported snapshots use birth
        # time where available, or mtime on systems without birth metadata.
        info = snapshot.stat()
        return datetime.fromtimestamp(getattr(info, "st_birthtime", info.st_mtime), timezone.utc)


def _copy_snapshot(source: Path, dest: Path) -> None:
    with closing(sqlite3.connect(source.resolve().as_uri() + "?mode=ro", uri=True)) as src:
        with closing(sqlite3.connect(dest)) as dst:
            src.backup(dst)


def build_seed_turns(p: Profile, snapshot: Path) -> list[Turn]:
    snapshot_at = _snapshot_time(snapshot)
    seed_dir = p.bench_dir / "seed"
    seed_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    turns = []
    with ExitStack() as stack:
        temporary = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix=".resolve-", dir=seed_dir)))
        resolvers = {}
        for index, seed in enumerate(load_seed_turns()):
            try:
                at = snapshot_at if seed.at == "snapshot" else datetime.fromisoformat(seed.at)
                _require_aware(at)
                key = (seed.stub, at)
                if key not in resolvers:
                    if seed.stub:
                        from assistant.bench.stub_dwt import StubDataset

                        client = StubDataset(seed.stub)
                    else:
                        state = temporary / str(index)
                        state.mkdir(mode=0o700)
                        _copy_snapshot(snapshot, state / "dwt.db")
                        client = stack.enter_context(DwtClient(p, state_dir=state, now=at))
                    resolvers[key] = _Resolver(client)
                resolver = resolvers[key]
                message = resolver.text(seed.message_template)
                history = [(role, resolver.text(text)) for role, text in seed.history]
                ops = [{"tool": item["tool"], "target": resolver.text(str(item["target"]))}
                       for item in seed.expect_ops]
                resolved = replace(seed, message_template=message, history=history, at=at.isoformat(), expect_ops=ops)
                turns.append(SeededTurn(seed.id, "seed", index, at, history, message, "", [], None, [],
                                        p.name, seed=resolved))
            except Exception as error:
                # MCP transport failures may be ExceptionGroups. The per-turn
                # boundary must also handle those without aborting the batch.
                # Do not log provider errors verbatim: they may contain mail.
                reason = (str(error) if isinstance(error, PlaceholderError)
                          else f"dati di prova non disponibili ({type(error).__name__})")
                logger.warning("Seed %s saltato: %s", seed.id, reason)
    payload = "".join(json.dumps(asdict(turn), ensure_ascii=False, default=lambda value: value.isoformat()) + "\n"
                      for turn in turns)
    _atomic_write(seed_dir / "turns.jsonl", payload)
    return turns
