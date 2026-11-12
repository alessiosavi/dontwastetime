"""Complete-cohort evaluation and the seed/promotion acceptance thresholds."""

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
import json
import math
from pathlib import Path

from assistant.bench import judge, rollout
from assistant.bench.evidence import freeze_evidence, require_evidence
from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore
from assistant.bench.seed import SeedTurn
from assistant.h2 import harness2
from assistant.profile import Profile
from assistant.sessions import Turn


class EvaluationIncomplete(RuntimeError):
    """A partial or invalid evaluation must never be used for promotion."""


@dataclass(frozen=True)
class GateResult:
    passed: bool
    live_mean: float
    cand_mean: float
    dim_drops: dict[str, float]
    new_failures: list[str]


def _cohort(scores: list[TurnScore]) -> dict[str, TurnScore]:
    if not scores:
        raise ValueError("La valutazione non contiene turni.")
    result = {}
    for score in scores:
        if not score.turn_id or score.turn_id in result:
            raise ValueError("Identificativi dei turni mancanti o duplicati.")
        if set(score.dims) != set(RUBRIC_DIMENSIONS) or any(
            type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= 10
            for value in [score.mean, *score.dims.values()]
        ):
            raise ValueError("Punteggi incompleti o non validi.")
        result[score.turn_id] = score
    return result


def _mean(scores: list[TurnScore]) -> float:
    return math.fsum(score.mean for score in scores) / len(scores)


def dimension_means(scores: list[TurnScore]) -> dict[str, float]:
    return {dim: math.fsum(score.dims[dim] for score in scores) / len(scores)
            for dim in RUBRIC_DIMENSIONS}


def regression_gate(live: list[TurnScore], cand: list[TurnScore]) -> GateResult:
    previous, candidate = _cohort(live), _cohort(cand)
    if previous.keys() != candidate.keys():
        raise ValueError("I due run devono valutare gli stessi turni.")
    live_dims, cand_dims = dimension_means(live), dimension_means(cand)
    drops = {dim: live_dims[dim] - cand_dims[dim] for dim in RUBRIC_DIMENSIONS}
    failures = [f"{identifier}: {failure}" for identifier, score in candidate.items()
                for failure in sorted(set(score.deterministic) - set(previous[identifier].deterministic))]
    live_mean, cand_mean = _mean(live), _mean(cand)
    return GateResult(cand_mean >= live_mean and all(drop <= 1 for drop in drops.values()) and not failures,
                      live_mean, cand_mean, drops, failures)


def seed_bar(scores: list[TurnScore]) -> bool:
    try:
        _cohort(scores)
    except ValueError:
        return False
    return _mean(scores) >= 7 and not any(score.deterministic for score in scores)


def evaluate(
    p: Profile, tree_dir: Path, turns: list[Turn], run_dir: Path, *,
    seeds: dict[str, SeedTurn] | None = None, seed_snapshot: Path | None = None, jobs: int = 4,
    frozen_evidence: dict[str, str] | None = None,
) -> list[TurnScore]:
    """Replay and grade at most jobs turns at once, returning input order.

    Each rollout retains its own resumable artifacts. Scores are returned only
    for a complete cohort; the caller must fail the gate on EvaluationIncomplete.
    """
    if type(jobs) is not int or jobs < 1:
        raise ValueError("jobs deve essere un intero positivo.")
    if not turns:
        raise EvaluationIncomplete("Nessun turno da valutare.")
    try:
        names = [rollout._turn_name(turn.turn_id) for turn in turns]
    except ValueError as error:
        raise EvaluationIncomplete("Identificativi dei turni non validi.") from error
    if len(set(names)) != len(names):
        raise EvaluationIncomplete("Identificativi dei turni duplicati o in conflitto.")
    tree_dir, run_dir = tree_dir.resolve(), run_dir.resolve()
    try:
        evidence = (freeze_evidence(p, turns, run_dir / "evidence.json") if frozen_evidence is None
                    else require_evidence(turns, frozen_evidence))
    except (OSError, ValueError, TypeError) as error:
        raise EvaluationIncomplete("Reazioni congelate non disponibili.") from error
    # Harness²'s first import temporarily changes the process cwd. Finish that
    # single-threaded initialization before any worker starts rendering.
    harness2()

    def evaluate_one(turn: Turn) -> TurnScore:
        try:
            results = rollout.execute_turns(p, tree_dir, [turn], run_dir,
                                            seeds=seeds, seed_snapshot=seed_snapshot)
            if len(results) != 1 or results[0].turn_id != turn.turn_id:
                raise EvaluationIncomplete(f"Rollout mancante per {turn.turn_id}.")
            done = json.loads((results[0].run_dir / "done.json").read_text(encoding="utf-8"))
            if done.get("failed"):
                raise EvaluationIncomplete(f"Rollout fallito per {turn.turn_id}.")
            score = judge.judge_turn(p, turn, results[0], evidence[turn.turn_id], seed=(seeds or {}).get(turn.turn_id))
            if score is None or score.turn_id != turn.turn_id:
                raise EvaluationIncomplete(f"Giudizio mancante per {turn.turn_id}.")
            _cohort([score])
            return score
        except EvaluationIncomplete:
            raise
        except Exception as error:
            # Provider/transport errors may include mail; expose only the type.
            raise EvaluationIncomplete(f"Turno {turn.turn_id} incompleto ({type(error).__name__}).") from error

    with ThreadPoolExecutor(max_workers=jobs) as pool:
        return list(pool.map(evaluate_one, turns))
