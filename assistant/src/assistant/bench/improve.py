"""One persistent training batch, a frozen regression gate, and human promotion."""

from contextlib import contextmanager
from dataclasses import asdict
from datetime import datetime, timezone
import fcntl
import hashlib
import json
from pathlib import Path
import re
import shutil

from assistant import harness_rules
from assistant.bench import gate
from assistant.bench.adapter import OperatorAdapter
from assistant.bench.report import write_report
from assistant.bench.seed import SeedTurn, SeededTurn, build_seed_turns, ensure_seed_snapshot
from assistant.h2 import harness2
from assistant.install import _atomic_write
from assistant.markers import load_markers, save_markers
from assistant.notes_mcp import _require_aware
from assistant.profile import Profile, _read_object
from assistant.sessions import TRIAGE_TOOLS, ToolCall, Turn, turns_from_export
from assistant.snapshot import snapshot_before
from assistant.spend import ROME, spend_between


_NEGATIVE = re.compile(r"\b(no|non|sbagliat\w*|errat\w*|più breve|più cortese|in inglese)\b", re.I)


@contextmanager
def _state_lock(p: Profile):
    # Ticks prune snapshots and update the same marker file. Never hold this
    # lock over model calls; the worker's improve.lock covers the run lifetime.
    with (p.root / "tick.lock").open("a", encoding="utf-8") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        yield


def fingerprint(directory: Path) -> str:
    tree = harness_rules.load_tree(directory)
    return hashlib.sha256(json.dumps(sorted(tree.items()), ensure_ascii=False).encode()).hexdigest()


def _json(path: Path, value) -> None:
    _atomic_write(path, json.dumps(value, ensure_ascii=False, indent=2, default=str) + "\n")


def _real_turns(p: Profile) -> list[Turn]:
    turns = {}
    for path in sorted(p.exports_dir.glob("*.json")):
        if path.name != "index.json":
            for turn in turns_from_export(path, p.name):
                if turn.profile == p.name:
                    turns[turn.turn_id] = turn
    return list(turns.values())


def assign_splits(p: Profile, turns: list[Turn]) -> dict[str, str]:
    """Assign a stable 20% holdout once; eviction never changes membership."""
    with _state_lock(p):
        path = p.bench_dir / "splits.json"
        splits = _read_object(path)
        if any(value not in ("train", "holdout") for value in splits.values()):
            raise ValueError("Split persistenti non validi.")
        for turn in turns:
            split = (turn.seed.split if getattr(turn, "seed", None) else
                     "holdout" if int(hashlib.sha256(turn.turn_id.encode()).hexdigest(), 16) % 5 == 0 else "train")
            splits.setdefault(turn.turn_id, split)
        _json(path, splits)
        return splits


def select_batch(turns: list[Turn], splits: dict[str, str]) -> list[Turn]:
    """The caller removes prior exposure using every previous run's batch_ids."""
    def priority(turn):
        negative = bool(_NEGATIVE.search(turn.next_message or ""))
        action = any(call.server == "dwt" and call.tool in TRIAGE_TOOLS for call in turn.tool_calls)
        return (negative, action, turn.at)

    return sorted((turn for turn in turns if splits.get(turn.turn_id) == "train"),
                  key=priority, reverse=True)[:25]


def _snapshot_pins(p: Profile, turns: list[Turn]) -> list[str]:
    return sorted({str(path) for turn in turns
                   if (path := (p.bench_dir / "seed/seed.db" if getattr(turn, "seed", None)
                                else snapshot_before(p, turn.at))) is not None})


def _pins(p: Profile, *, regression: list[str] | None = None,
          run_id: str | None = None, snapshots: list[str] | None = None) -> None:
    """Replace our owned pins while preserving pins registered by other users."""
    path = p.bench_dir / "pins.json"
    data = _read_object(path)
    runs = data.setdefault("runs", {})
    owned = set(data.get("regression", [])) | {pin for pins in runs.values() for pin in pins}
    external = set(data.get("snapshots", [])) - owned
    if regression is not None:
        data["regression"] = regression
    if run_id is not None:
        if snapshots is None:
            runs.pop(run_id, None)
        else:
            runs[run_id] = snapshots
    data["snapshots"] = sorted(external | set(data.get("regression", [])) |
                               {pin for pins in runs.values() for pin in pins})
    _json(path, data)


def _regression(p: Profile, real: list[Turn], seeds: list[Turn], splits: dict[str, str]) -> list[Turn]:
    holdout = [turn for turn in seeds if splits[turn.turn_id] == "holdout"]
    if len(holdout) > 60:
        raise ValueError("Il set iniziale holdout supera 60 turni.")
    newest = sorted((turn for turn in real if splits[turn.turn_id] == "holdout"),
                    key=lambda turn: turn.at, reverse=True)
    return holdout + newest[:60 - len(holdout)]


def update_regression_set(p: Profile) -> list[Turn]:
    real = _real_turns(p)
    seeds = build_seed_turns(p, ensure_seed_snapshot(p))
    splits = assign_splits(p, [*real, *seeds])
    with _state_lock(p):
        turns = _regression(p, real, seeds, splits)
        _pins(p, regression=_snapshot_pins(p, turns))
    return turns


def _freeze(turns: list[Turn]) -> list[dict]:
    return [asdict(turn) for turn in turns]


def _thaw(rows: list[dict]) -> list[Turn]:
    turns = []
    for row in rows:
        row = dict(row)
        row["at"] = datetime.fromisoformat(row["at"])
        if row.get("completed_at"):
            row["completed_at"] = datetime.fromisoformat(row["completed_at"])
        row["history"] = [tuple(pair) for pair in row["history"]]
        for key in ("tool_calls", "following_tool_calls"):
            row[key] = [ToolCall(**call) for call in row[key]]
        if row.get("seed"):
            row["seed"] = SeedTurn(**row["seed"])
            turns.append(SeededTurn(**row))
        else:
            row.pop("seed", None)
            turns.append(Turn(**row))
    return turns


def _run_dir(p: Profile, run_id: str) -> Path:
    if not re.fullmatch(r"[A-Za-z0-9_-]+", run_id):
        raise ValueError("Identificativo del run non valido.")
    path = p.bench_dir / "runs" / run_id
    if path.is_symlink():
        raise ValueError("Il run non può essere un collegamento simbolico.")
    return path


def run_task(*args, **kwargs):
    # Safe for unit callers too; in production the worker already imported H²
    # from its prepared root. Never import a Harness² module at module scope.
    harness2()
    from harness2.core.schedule import run_task as h2_run_task

    return h2_run_task(*args, **kwargs)


def _propose(p: Profile, state: dict, batch: list[Turn], seeds: dict[str, SeedTurn], snapshot: Path):
    h2 = harness2()
    from harness2.core.library import Library
    from harness2.core.proposer import Proposer
    from harness2.core.schedule import Namespace
    from harness2.core.spec import ChannelSpec

    adapter = OperatorAdapter(p, batch, seeds, batch_id=state["batch_id"], seed_snapshot=snapshot)
    root = p.bench_dir / "h2/runs/operator"
    result = run_task(state["batch_id"], 1, adapter=adapter,
                      prop=Proposer(ChannelSpec(kind="opencode", model=p.settings.model, timeout=1800)),
                      runs_root=root, modes=("agg",), tag=state["tag"],
                      library=Library(root=p.bench_dir / "library", domain="operator", surface=h2.OPENCODE_SURFACE))
    selection = result.selections.get("agg") or {}
    label = selection.get("chosen_label")
    if not selection.get("ok") or not isinstance(label, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", label):
        raise ValueError("Harness² non ha scelto un candidato valido.")
    work = Namespace(adapter.spec.name, adapter.substrate.name, "agg", 1, tag=state["tag"]).task_dir(
        root, state["batch_id"])
    directory = work / "harnesses" / label
    if not directory.is_dir() or directory.is_symlink():
        raise ValueError("L'albero scelto da Harness² non è disponibile.")
    tree = harness_rules.load_tree(directory)
    harness_rules.validate_tree(tree)
    return tree


def _finish(p: Profile, run_id: str) -> None:
    with _state_lock(p):
        _pins(p, run_id=run_id)
        markers = load_markers(p)
        if markers.improve_run == run_id:
            markers.improve_run = None
            save_markers(p, markers)


def _summary(before: dict[str, str], after: dict[str, str]) -> str:
    h2 = harness2()
    diff = h2.tree.diff_summary(h2.OPENCODE_SURFACE, before, after)
    labels = {"systemprompt.md": "istruzioni per le risposte",
              "guardrails.md": "indicazioni sulle azioni consentite",
              "LongTermMEMORY.md": "memoria di lungo periodo",
              "tool_descriptions/tool_guidance.md": "indicazioni per consultare la posta"}
    changed = set(diff["added"] + diff["modified"] + diff["removed"])
    parts = [label for path, label in labels.items() if path in changed]
    if any(path.startswith("skills/") for path in changed):
        parts.append("procedure per le richieste di Operator")
    return "ho aggiornato " + ", ".join(parts) + "." if parts else "Nessuna modifica al testo."


def run_improvement(p: Profile, now: datetime, *, max_batch: int = 25, max_regression: int = 60) -> Path:
    """Called inside the isolated worker; interrupted calls retain their receipt."""
    _require_aware(now)
    with _state_lock(p):
        markers = load_markers(p)
        markers.last_improve_start = now
        save_markers(p, markers)  # FIRST, even if cohort preparation crashes.
        run_id = markers.improve_run
    if (type(max_batch) is not int or not 1 <= max_batch <= 25
            or type(max_regression) is not int or not 1 <= max_regression <= 60):
        raise ValueError("Limiti richiesti: batch 1–25, regressione 1–60.")
    if run_id:
        directory = _run_dir(p, run_id)
        if (directory / "GATE").is_file():
            _finish(p, run_id)
            return directory
        state = _read_object(directory / "run.json")
        frozen = _read_object(directory / "turns.json")
        batch, regression = _thaw(frozen["batch"]), _thaw(frozen["regression"])
    else:
        real = _real_turns(p)
        snapshot = ensure_seed_snapshot(p)
        seeds = build_seed_turns(p, snapshot)
        splits = assign_splits(p, [*real, *seeds])
        used = {identifier for path in (p.bench_dir / "runs").glob("*/run.json")
                for identifier in _read_object(path)["batch_ids"]}
        batch = select_batch([turn for turn in real if turn.turn_id not in used], splits)[:max_batch]
        if not batch:
            batch = sorted((turn for turn in seeds
                            if splits[turn.turn_id] == "train" and turn.turn_id not in used),
                           key=lambda turn: turn.turn_id)[:max_batch]
        regression = _regression(p, real, seeds, splits)[:max_regression]
        run_id = now.astimezone(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
        directory = _run_dir(p, run_id)
        directory.mkdir(mode=0o700, parents=True)
        state = {"batch_id": run_id, "batch_ids": [turn.turn_id for turn in batch],
                 "regression_ids": [turn.turn_id for turn in regression],
                 "live_fingerprint": fingerprint(p.root / "harness-live"), "tag": "operator-" + run_id,
                 "started": now.astimezone(timezone.utc).isoformat(), "seed_snapshot": str(snapshot),
                 "snapshots": _snapshot_pins(p, [*batch, *regression]),
                 "splits": {turn.turn_id: splits[turn.turn_id] for turn in [*batch, *regression]}}
        _json(directory / "turns.json", {"batch": _freeze(batch), "regression": _freeze(regression)})
        _json(directory / "run.json", state)
        with _state_lock(p):
            _pins(p, regression=_snapshot_pins(p, regression))
    with _state_lock(p):
        _pins(p, run_id=run_id, snapshots=state["snapshots"])
        markers = load_markers(p)
        markers.improve_run = run_id
        save_markers(p, markers)
    seeds = {turn.turn_id: turn.seed for turn in [*batch, *regression] if getattr(turn, "seed", None)}
    live, candidate_scores, result, tree = [], [], None, {}
    failure = None
    try:
        if not batch:
            raise ValueError("Nessun nuovo turno di training disponibile.")
        if fingerprint(p.root / "harness-live") != state["live_fingerprint"]:
            raise ValueError("Il live è cambiato: il confronto congelato non è più valido.")
        tree = _propose(p, state, batch, seeds, Path(state["seed_snapshot"]))
        # Publish the validated candidate before evaluating it. A crash can
        # resume Harness²'s cached selection without changing the frozen cohort.
        candidate = directory / "candidate"
        if candidate.exists():
            shutil.rmtree(candidate)
        for name, text in tree.items():
            _atomic_write(candidate / name, text)
        for label, target, scores in (("live", p.root / "harness-live", live),
                                      ("candidate", candidate, candidate_scores)):
            scores.extend(gate.evaluate(p, target, regression, directory / f"{run_id}-{label}",
                                        seeds=seeds, seed_snapshot=Path(state["seed_snapshot"])))
        if fingerprint(p.root / "harness-live") != state["live_fingerprint"]:
            raise ValueError("Il live è cambiato durante la valutazione.")
        result = gate.regression_gate(live, candidate_scores)
    except (ValueError, gate.EvaluationIncomplete) as error:
        failure = str(error)
    passed = result is not None and result.passed and failure is None
    ended = datetime.now(timezone.utc)
    report = write_report(directory, live=live, cand=candidate_scores, gate=result, passed=passed,
                          incomplete=failure is not None, splits=state["splits"],
                          cost=spend_between(p, datetime.fromisoformat(state["started"]), ended))
    summary = "Nessuna modifica proposta."
    if tree:
        summary = _summary(harness_rules.load_tree(p.root / "harness-live"), tree)
    extra = f"\n## Sintesi\n\nHo imparato: {summary}\n"
    if failure:
        extra += f"\nMotivo del gate fallito: {failure}\n"
    month_start = ended.astimezone(ROME).replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    if spend_between(p, month_start, ended).usd > p.settings.soft_cap_usd:
        extra += f"\n⚠ sopra la soglia mensile di ${p.settings.soft_cap_usd:g}.\n"
    _atomic_write(report, report.read_text(encoding="utf-8") + extra)
    (directory / "candidate").mkdir(mode=0o700, exist_ok=True)
    _atomic_write(directory / "GATE", "passed\n" if passed else "failed\n")  # LAST artifact.
    _finish(p, run_id)
    return directory


def promote(p: Profile, run_id: str, repo: Path) -> list[Path]:
    directory = _run_dir(p, run_id)
    if not (directory / "GATE").is_file() or (directory / "GATE").read_text(encoding="utf-8").strip() != "passed":
        raise ValueError("Promozione rifiutata: il GATE non è passed.")
    source = directory / "candidate"
    if not source.is_dir() or source.is_symlink():
        raise ValueError("Candidato non disponibile.")
    after = harness_rules.load_tree(source)
    harness_rules.validate_tree(after)
    target = Path(repo).expanduser().resolve() / "assistant/harness"
    if target.is_symlink() or target.parent.is_symlink():
        raise ValueError("La destinazione non può essere un collegamento simbolico.")
    before = harness_rules.load_tree(target)
    changed = [target / name for name in sorted(before.keys() | after.keys()) if before.get(name) != after.get(name)]
    for name in before.keys() - after.keys():
        (target / name).unlink()
    for name, text in after.items():
        if before.get(name) != text:
            _atomic_write(target / name, text)
    return changed


def record_promoted_install(p: Profile, previous: str | None, now: datetime) -> None:
    """Announce only a changed installation matching a passed candidate."""
    from assistant.status import record_status

    current = fingerprint(p.root / "harness-live")
    if previous is None or previous == current:
        return
    for directory in sorted((p.bench_dir / "runs").glob("*"), reverse=True):
        try:
            if (directory / "GATE").read_text(encoding="utf-8").strip() != "passed":
                continue
            if fingerprint(directory / "candidate") != current:
                continue
            summary = next(line for line in (directory / "report.md").read_text(encoding="utf-8").splitlines()
                           if line.startswith("Ho imparato: "))
        except (OSError, ValueError, StopIteration):
            continue
        record_status(p, f"promoted:{directory.name}", summary, now)
        break
