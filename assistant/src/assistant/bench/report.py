"""Metadata-only Markdown summaries: no replies, reasons or mailbox content."""

from html import escape
from pathlib import Path

from assistant.bench.gate import dimension_means
from assistant.bench.judge import RUBRIC_DIMENSIONS
from assistant.install import _atomic_write


def _cell(value) -> str:
    return escape(str(value)).replace("|", "&#124;").replace("\n", " ").replace("\r", " ")


def write_report(run_dir: Path, **sections) -> Path:
    """Write report.md from scores, passed, splits, skipped and cost.

    Optional live/cand cohorts and a GateResult in gate support regression runs.
    Unknown sections and TurnScore.reasons are deliberately never rendered.
    """
    scores = sections.get("scores", sections.get("cand", []))
    live = sections.get("live", [])
    gate = sections.get("gate")
    incomplete = sections.get("incomplete", False)
    passed = (gate.passed if gate is not None else sections.get("passed", False)) and not incomplete
    lines = ["# Valutazione assistente", "", "PASS" if passed else "FAIL", ""]
    mean = sum(score.mean for score in scores) / len(scores) if scores else None
    lines.append(f"Media complessiva: {mean:.2f}/10" if mean is not None else "Media complessiva: non disponibile")
    if live:
        lines.append(f"Media live: {sum(score.mean for score in live) / len(live):.2f}/10")
    if incomplete:
        lines.extend(["", "Valutazione incompleta: il run non può superare il gate."])
    lines.extend(["", "## Dimensioni", "", "| Dimensione | Media |" + (" Live |" if live else ""),
                  "| --- | ---: |" + (" ---: |" if live else "")])
    means = dimension_means(scores) if scores else {}
    live_means = dimension_means(live) if live else {}
    for dim in RUBRIC_DIMENSIONS:
        value = f"{means[dim]:.2f}" if means else "—"
        lines.append(f"| {dim} | {value} |" + (f" {live_means[dim]:.2f} |" if live else ""))
    splits = sections.get("splits", {})
    cohorts = [("live", live), ("candidato", scores)] if live else [("", scores)]
    for label, cohort in cohorts:
        lines.extend(["", f"## Turni{(' ' + label) if label else ''}", "",
                      "| Turno | Split | Media | Dimensioni | Fallimenti deterministici |",
                      "| --- | --- | ---: | --- | --- |"])
        for score in cohort:
            dims = ", ".join(f"{dim}: {score.dims[dim]:.2f}" for dim in RUBRIC_DIMENSIONS)
            failures = ", ".join(score.deterministic) or "nessuno"
            lines.append(f"| {_cell(score.turn_id)} | {_cell(splits.get(score.turn_id, '—'))} | "
                         f"{score.mean:.2f} | {dims} | {_cell(failures)} |")
    if gate is not None:
        lines.extend(["", "## Nuovi fallimenti", ""])
        lines.extend(f"- {_cell(failure)}" for failure in gate.new_failures)
        if not gate.new_failures:
            lines.append("Nessuno.")
    lines.extend(["", "## Turni saltati", ""])
    skipped = sections.get("skipped", [])
    lines.extend([f"- {_cell(identifier)}" for identifier in skipped] or ["Nessuno."])
    lines.extend(["", "## Costo", ""])
    cost = sections.get("cost")
    if cost is None:
        lines.append("Costo non disponibile.")
    elif cost.unknown_tokens:
        lines.append(f"Costo non disponibile per {cost.unknown_tokens} token; costo noto: ${cost.usd:.6f}.")
    else:
        lines.append(f"Costo: ${cost.usd:.6f}.")
    path = run_dir / "report.md"
    _atomic_write(path, "\n".join(lines) + "\n")
    return path
