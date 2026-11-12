"""Worker process import order and inter-process exclusion."""

import ast
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest


def python(code, *args, cwd=None, env=None, timeout=30):
    return subprocess.run([sys.executable, "-c", code, *map(str, args)], cwd=cwd,
                          env=env, capture_output=True, text=True, timeout=timeout)


def test_proposer_finds_checkout(profile, tmp_path):
    from assistant.bench.shim import prepare_harness2_root

    root = prepare_harness2_root(profile)
    # A parent .env must not become Harness²'s ROOT/.env.
    (tmp_path / ".env").write_text("H2_POISON=loaded\n")
    result = python("""
import json, os
from harness2 import config
from harness2.core.proposer import _opencode_dir
print(json.dumps({'ROOT':str(config.ROOT), 'BUN':str(config.BUN),
                  'opencode':str(_opencode_dir()), 'poison':os.environ.get('H2_POISON')}))
""", cwd=root)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout) == {
        "ROOT": str(root), "BUN": str(root / "dependencies/tools/node_modules/.bin/bun"),
        "opencode": str(root / "dependencies/opencode"), "poison": None}


def test_worker_imports_harness2_after_chdir(profile):
    from assistant.bench import adapter, shim, worker

    for module in (adapter, shim, worker):
        tree = ast.parse(Path(module.__file__).read_text())
        top_level = [node for node in tree.body if isinstance(node, (ast.Import, ast.ImportFrom))]
        assert not any(
            (node.module or "").startswith("harness2") if isinstance(node, ast.ImportFrom)
            else any(name.name.startswith("harness2") for name in node.names)
            for node in top_level)
    result = python("""
import fcntl, json, os, sys, types
from pathlib import Path
from assistant.bench import adapter, shim, worker
assert 'harness2' not in sys.modules
def run(p, now):
    assert now.tzinfo is not None
    from harness2 import config
    from harness2.core.proposer import _opencode_dir
    assert 'harness2.core.schedule' in sys.modules
    with (p.root/'improve.lock').open('a') as lock:
        try: fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError: pass
        else: raise AssertionError('worker lock was released early')
    print(json.dumps({'root':str(config.ROOT), 'runs':str(config.RUNS),
          'models':[config.SOLVER_MODEL_OC, config.IMPROVER_MODEL, config.JUDGE_MODEL],
          'opencode':str(_opencode_dir())}))
    directory = p.bench_dir/'runs/fake-run'
    directory.mkdir(parents=True)
    (directory/'GATE').write_text('passed')
    return directory
sys.modules['assistant.bench.improve'] = types.SimpleNamespace(run_improvement=run)
raise SystemExit(worker.improve_worker_main('dev', Path(sys.argv[1])))
""", profile.root.parent.parent)
    assert result.returncode == 0, result.stderr
    root = profile.bench_dir / "h2"
    assert json.loads(result.stdout.splitlines()[0]) == {
        "root": str(root), "runs": str(root / "runs"),
        "models": [profile.settings.model] * 3, "opencode": str(root / "dependencies/opencode")}


def test_worker_exits_when_lock_held(profile):
    with (profile.root / "improve.lock").open("a") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        result = python("""
import sys
from pathlib import Path
from assistant.bench.worker import improve_worker_main
assert improve_worker_main('dev', Path(sys.argv[1])) == 0
assert 'harness2' not in sys.modules
""", profile.root.parent.parent)
    assert result.returncode == 0, result.stderr
    assert not (profile.bench_dir / "h2").exists()


def test_worker_cli_runs_improvement(profile):
    result = python("""
import sys, types
from pathlib import Path
from assistant.cli import main
def run(p, now):
    assert now.tzinfo is not None
    directory = p.bench_dir/'runs/fake-run'
    directory.mkdir(parents=True)
    (directory/'GATE').write_text('passed')
    (directory/'report.md').write_text('report')
    return directory
sys.modules['assistant.bench.improve'] = types.SimpleNamespace(run_improvement=run)
raise SystemExit(main(['bench', 'improve-worker', '--profile', 'dev', '--state-root', sys.argv[1]]))
""", profile.root.parent.parent)
    assert result.returncode == 0, result.stderr
    assert "fake-run: GATE=passed" in result.stdout and "report.md" in result.stdout
    assert (profile.bench_dir / "h2/dependencies/tools/node_modules/.bin/bun").is_file()


def test_proposer_calls_unmodified_through_shim(profile, tmp_path, event_runner):
    from assistant.bench.shim import prepare_harness2_root

    log = event_runner()
    root = prepare_harness2_root(profile)
    result = python("""
import json, os, sys
os.chdir(sys.argv[1])
from harness2.core.proposer import Proposer
from harness2.core.spec import ChannelSpec
reply = Proposer(ChannelSpec(kind='opencode', model=sys.argv[2]), attempts=1).call(
    'Rispondi solo: ok', None, 'fake-spike')
print(json.dumps({'text':reply.text, 'cost':reply.cost_usd}))
""", root, profile.settings.model)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["text"].endswith("DONE")
    call = json.loads(log.read_text())
    assert call["argv"][:6] == ["run", "--standalone", "--format", "json", "--model", profile.settings.model]
    assert call["argv"][-2] == "--"
    assert call["env"]["XDG_DATA_HOME"].endswith("/.ocdata")
    usage = json.loads((profile.root / "usage.jsonl").read_text())
    assert usage["call_id"].startswith("improver:ses_")
    assert usage["tokens_in"] == 1420


@pytest.mark.integration
@pytest.mark.skipif(not os.environ.get("ASSISTANT_IT_STATE_ROOT"), reason="requires an installed dev profile")
def test_proposer_real_call_through_shim(monkeypatch):
    # Keep the real PATH captured at collection; conftest replaces it for unit tests.
    result = python("""
import json, os, sys
from pathlib import Path
from assistant.profile import load_profile
from assistant.bench.shim import prepare_harness2_root
p = load_profile('dev', Path(sys.argv[1]))
root = prepare_harness2_root(p)
os.chdir(root)
os.environ.update(HARNESS2_RUNS_DIR=str(root/'runs'), HARNESS2_SOLVER_MODEL=p.settings.model,
                  HARNESS2_IMPROVER_MODEL=p.settings.model, HARNESS2_JUDGE_MODEL=p.settings.model)
from harness2.core.proposer import Proposer
from harness2.core.spec import ChannelSpec
reply = Proposer(spec=ChannelSpec(kind='opencode', model=p.settings.model, timeout=1800),
                 attempts=1).call('Rispondi solo: ok', None, 'spike')
assert reply.text.strip() and reply.cost_usd >= 0
print(json.dumps({'text':reply.text, 'cost':reply.cost_usd}))
""", IT_STATE_ROOT, env=dict(os.environ, PATH=REAL_PATH), timeout=1850)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout)["text"].strip()


REAL_PATH = os.environ.get("PATH", "")
IT_STATE_ROOT = os.environ.get("ASSISTANT_IT_STATE_ROOT", "")
