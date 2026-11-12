"""Persist judging inputs before any rollout or proposer can incur spend."""

import json
from pathlib import Path

from assistant.bench import judge
from assistant.install import _atomic_write
from assistant.profile import Profile
from assistant.sessions import Turn
from assistant.snapshot import _snapshots


def require_evidence(turns: list[Turn], evidence: dict[str, str]) -> dict[str, str]:
    if (not isinstance(evidence, dict) or any(not isinstance(value, str) for value in evidence.values())
            or any(turn.turn_id not in evidence for turn in turns)):
        raise ValueError("Reazioni congelate mancanti o non valide.")
    return dict(evidence)


def freeze_evidence(p: Profile, turns: list[Turn], path: Path) -> dict[str, str]:
    if path.exists():
        return require_evidence(turns, json.loads(path.read_text(encoding="utf-8")))
    snapshots = _snapshots(p)
    if not snapshots:
        raise ValueError("Nessuna snapshot disponibile per le reazioni di Operator.")
    latest = snapshots[-1][1]
    evidence = {turn.turn_id: judge.evidence_for(turn, latest=latest)
                for turn in {turn.turn_id: turn for turn in turns}.values()}
    require_evidence(turns, evidence)
    _atomic_write(path, json.dumps(evidence, ensure_ascii=False, indent=2) + "\n")
    return evidence
