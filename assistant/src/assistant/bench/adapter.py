"""Operator training batches at Harness²'s BenchmarkAdapter seam."""

from functools import cached_property
import hashlib
import json
from pathlib import Path

from assistant.bench import judge, rollout
from assistant.bench.checks import RolloutTurn
from assistant.bench.evidence import freeze_evidence, require_evidence
from assistant.bench.seed import SeedTurn
from assistant.h2 import harness2
from assistant import harness_rules
from assistant.notes_mcp import _atomic_write
from assistant.profile import Profile
from assistant.sessions import ToolCall, Turn
from assistant.snapshot import snapshot_before


class OperatorAdapter:
    def __init__(self, p: Profile, batch: list[Turn], seeds: dict[str, SeedTurn], *, batch_id: str,
                 seed_snapshot: Path | None = None, frozen_evidence: dict[str, str] | None = None):
        self.p, self.batch, self.seeds, self.batch_id = p, list(batch), dict(seeds), batch_id
        self.seed_snapshot = seed_snapshot
        self.frozen_evidence = None if frozen_evidence is None else require_evidence(batch, frozen_evidence)
        names = [rollout._turn_name(turn.turn_id) for turn in batch]
        if len(set(names)) != len(names):
            raise ValueError("Identificativi dei turni in conflitto dopo la normalizzazione.")

    @cached_property
    def spec(self):
        harness2()  # Outside the worker, initialize from an empty cwd (A9).
        from harness2.core.spec import BenchPrompts, BenchSpec, ChannelSpec

        channel = ChannelSpec(kind="opencode", model=self.p.settings.model, timeout=1800)
        return BenchSpec(
            name="operator", runs_root=self.p.bench_dir / "h2/runs",
            install_mode="workspace_agents_md", allow_subagents=False,
            prompts=BenchPrompts(work="rispondere a Operator in italiano usando dwt",
                                domain_word="assistente", deliverable_word="risposte"),
            channels={"proposer": channel, "selector": channel},
        )

    @cached_property
    def substrate(self):
        return harness2().OpencodeSubstrate()

    def _task(self, task_id: str) -> None:
        if task_id != self.batch_id:
            raise ValueError(f"Batch non disponibile: {task_id}")

    def tasks(self, split: str = "all") -> list[str]:
        return [self.batch_id]

    def domain_of(self, task_id: str) -> str:
        self._task(task_id)
        return "operator"

    def task_prompt(self, task_id: str) -> str:
        self._task(task_id)
        return "\n\n".join(f"## Turno {turn.turn_id}\n\n{rollout.transcript(turn)}" for turn in self.batch)

    @cached_property
    def _evidence(self) -> dict[str, str]:
        if self.frozen_evidence is not None:
            return self.frozen_evidence
        key = hashlib.sha256(self.batch_id.encode()).hexdigest()
        return freeze_evidence(self.p, self.batch, self.p.bench_dir / "batches" / key / "evidence.json")

    def task_files(self, task_id: str) -> Path | None:
        self._task(task_id)
        key = hashlib.sha256(self.batch_id.encode()).hexdigest()
        directory = self.p.bench_dir / "batches" / key / "evidence"
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        text = "\n\n".join(f"## Turno {turn.turn_id}\n\n{self._evidence[turn.turn_id]}"
                           for turn in self.batch)
        _atomic_write(directory / "reazioni.md", text + "\n")
        return directory

    def execute(
        self, task_id: str, harness_dir: Path | None, run_id: str, *, timeout: int | None = None,
    ) -> Path:
        self._task(task_id)
        self._evidence  # Freeze before the first paid rollout, including standalone adapter callers.
        root = self.spec.runs_root.resolve()
        directory = (root / run_id).resolve()
        if directory == root or not directory.is_relative_to(root):
            raise ValueError("La directory del run deve restare dentro Harness² runs.")
        seed_turns = [turn for turn in self.batch if turn.turn_id in self.seeds or getattr(turn, "seed", None)]
        seed_snapshot = None
        if seed_turns:
            seed_snapshot = self.seed_snapshot or snapshot_before(self.p, min(turn.at for turn in seed_turns))
        rollout.execute_turns(self.p, harness_dir if harness_dir is not None else self.base_harness(),
                              self.batch, directory, seeds=self.seeds, seed_snapshot=seed_snapshot)
        return directory

    def _directory(self, run_dir: Path, turn: Turn) -> Path:
        return Path(run_dir) / rollout._turn_name(turn.turn_id)

    def delivered(self, run_dir: Path) -> bool:
        return bool(self.batch) and all((self._directory(run_dir, turn) / "reply.md").is_file()
                                        for turn in self.batch)

    def trajectory(self, run_dir: Path, *, obs_trunc: int) -> str:
        return "\n\n".join(
            f"## Turno {turn.turn_id}\n\n" + self.substrate.read_trajectory(
                self._directory(run_dir, turn) / "events.jsonl", obs_trunc=obs_trunc, max_chars=8_000_000)
            for turn in self.batch)

    def deliverable_files(self, run_dir: Path) -> dict[str, str]:
        files = {}
        for turn in self.batch:
            path = self._directory(run_dir, turn) / "reply.md"
            if path.is_file():
                files[path.relative_to(run_dir).as_posix()] = path.read_text(encoding="utf-8")
        return files

    def deliverable_text(self, run_dir: Path) -> str:
        files = self.deliverable_files(run_dir)
        return "\n\n".join(f"## Turno {turn.turn_id}\n\n{files[name]}" for turn in self.batch
                           if (name := f"{rollout._turn_name(turn.turn_id)}/reply.md") in files)

    def base_harness(self) -> Path:
        return self.p.root / "harness-live"

    def validate_tree(self, tree: dict[str, str]) -> None:
        harness_rules.validate_tree(tree)

    def score(self, run_dir: Path, task_id: str) -> dict | None:
        self._task(task_id)
        if not self.batch:
            return None
        results = []
        try:
            for turn in self.batch:
                directory = self._directory(run_dir, turn)
                json.loads((directory / "done.json").read_text(encoding="utf-8"))
                calls = json.loads((directory / "tool_calls.json").read_text(encoding="utf-8"))
                results.append(RolloutTurn(turn.turn_id, (directory / "reply.md").read_text(encoding="utf-8"),
                                           [ToolCall(**call) for call in calls], directory))
            evidence = self._evidence
        except (OSError, ValueError, TypeError):
            return None
        scores = []
        for turn, result in zip(self.batch, results, strict=True):
            score = judge.judge_turn(self.p, turn, result, evidence[turn.turn_id],
                                     seed=self.seeds.get(turn.turn_id))
            if score is None:
                return None
            scores.append(score)
        return {"n_passed": sum(value >= 7 for score in scores for value in score.dims.values()),
                "n_criteria": 5 * len(scores), "mean": sum(score.mean for score in scores) / len(scores)}
