"""Synthetic replay inputs and an executable OpenCode event emitter."""

from datetime import datetime, timezone
import json
from pathlib import Path
import sqlite3
import sys

import pytest

from assistant.sessions import Turn


@pytest.fixture
def bench_turn():
    return Turn("session:0", "session", 0, datetime(2026, 11, 12, 9, tzinfo=timezone.utc),
                [("user", "ciao"), ("assistant", "Buongiorno")], "cosa devo fare oggi?",
                "", [], None, [], "dev")


@pytest.fixture
def bench_snapshot(profile):
    profile.snapshots_dir.mkdir(parents=True)
    snapshot = profile.snapshots_dir / "dwt-20261112T083000Z.db"
    with sqlite3.connect(snapshot) as db:
        db.execute("CREATE TABLE synthetic (value TEXT)")
        db.execute("INSERT INTO synthetic VALUES ('original')")
    return snapshot


@pytest.fixture
def event_runner(fake_opencode, tmp_path):
    invocations = tmp_path / "event-invocations.jsonl"
    fixture = (Path(__file__).resolve().parents[1] / "fixtures/run_events.jsonl").read_text()

    def configure(stdout=fixture, *, exit_code=0, mutate=False):
        fake_opencode.path.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, sqlite3, sys\n"
            "data = {'argv': sys.argv[1:], 'env': dict(os.environ), 'cwd': os.getcwd()}\n"
            f"with open({str(invocations)!r}, 'a') as f: f.write(json.dumps(data) + '\\n')\n"
            + ("with sqlite3.connect('../dwt/dwt.db') as db: "
               "db.execute(\"UPDATE synthetic SET value='changed'\")\n" if mutate else "")
            + f"sys.stdout.write({stdout!r})\nsys.exit({exit_code})\n"
        )
        return invocations

    return configure
