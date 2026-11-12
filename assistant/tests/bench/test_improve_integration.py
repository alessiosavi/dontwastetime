"""Opt-in paid improvement on the installed dev profile; Claude runs it."""

import os
import subprocess
import sys

import pytest

REAL_PATH = os.environ.get("PATH", "")
STATE_ROOT = os.environ.get("ASSISTANT_IT_STATE_ROOT")


@pytest.mark.integration
@pytest.mark.skipif(not STATE_ROOT, reason="ASSISTANT_IT_STATE_ROOT must point at an installed dev profile")
def test_improvement_end_to_end_small():
    # A fresh interpreter is essential: pytest has already safely imported H²
    # under an empty cwd for unit tests, whereas a worker captures its own root.
    result = subprocess.run([sys.executable, "-c", '''
from datetime import datetime, timezone
import os
from pathlib import Path
import sys
from assistant.profile import load_profile
from assistant.bench.shim import prepare_harness2_root
p = load_profile("dev", Path(sys.argv[1]))
root = prepare_harness2_root(p)
os.chdir(root)
os.environ.update(HARNESS2_RUNS_DIR=str(root / "runs"),
                  HARNESS2_SOLVER_MODEL=p.settings.model,
                  HARNESS2_IMPROVER_MODEL=p.settings.model,
                  HARNESS2_JUDGE_MODEL=p.settings.model)
from harness2.core import schedule, proposer
from assistant.bench.improve import run_improvement
import fcntl
with (p.root / "improve.lock").open("a") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    directory = run_improvement(p, datetime.now(timezone.utc), max_batch=3, max_regression=3)
assert (directory / "run.json").is_file()
assert (directory / "report.md").is_file()
assert (directory / "candidate").is_dir()
assert (directory / "GATE").read_text().strip() in ("passed", "failed")
print(directory)
''', STATE_ROOT], env=dict(os.environ, PATH=REAL_PATH), capture_output=True, text=True, timeout=14400)
    assert result.returncode == 0, result.stderr[-4000:]
    assert result.stdout.strip()
