"""Dedicated improvement process: lock and configure cwd before importing Harness²."""

import fcntl
import importlib
from datetime import datetime, timezone
import os
from pathlib import Path

from assistant.bench.shim import prepare_harness2_root
from assistant.profile import load_profile


def improve_worker_main(profile: str, state_root: Path | None = None) -> int:
    p = load_profile(profile, state_root)
    with (p.root / "improve.lock").open("a", encoding="utf-8") as lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print("Un miglioramento è già in corso.")
            return 0
        root = prepare_harness2_root(p)
        os.chdir(root)
        os.environ.update(
            HARNESS2_RUNS_DIR=str(root / "runs"), HARNESS2_SOLVER_MODEL=p.settings.model,
            HARNESS2_IMPROVER_MODEL=p.settings.model, HARNESS2_JUDGE_MODEL=p.settings.model,
        )
        # config.py captures cwd, .env and model settings at first import.
        importlib.import_module("harness2.core.schedule")
        importlib.import_module("harness2.core.proposer")
        improve = importlib.import_module("assistant.bench.improve")
        directory = improve.run_improvement(p, datetime.now(timezone.utc))
        gate = (directory / "GATE").read_text(encoding="utf-8").strip()
        print(f"Run {directory.name}: GATE={gate}; report: {directory / 'report.md'}")
        return 0 if gate == "passed" else 1
