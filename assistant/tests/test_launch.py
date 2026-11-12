"""Launch a real fake HTTP child, with no OpenCode, browser or credentials."""

from dataclasses import replace
import fcntl
import json
import os
from pathlib import Path
import selectors
import signal
import subprocess
import sys
import time
from types import ModuleType

import pytest

from assistant.profile import load_profile
from assistant.render import render_workspace


HARNESS = Path(__file__).resolve().parents[1] / "harness"
PASSWORD = "ephemeral-test-password"
PAIR_CODE = "test-pair-code"


@pytest.fixture
def ui(tmp_path, fake_tools):
    p = load_profile("dev", state_root=tmp_path / "state")
    p.settings.secrets_env.write_text("AZURE_API_KEY=fake-file-key\n")
    import shutil

    shutil.copytree(HARNESS, p.root / "harness-live")
    p = replace(p, settings=replace(p.settings, opencode_bin=str(fake_tools["opencode"].path)))
    (p.root / "settings.json").write_text(json.dumps({
        "opencode_bin": p.settings.opencode_bin, "secrets_env": str(p.settings.secrets_env),
    }))
    record = tmp_path / "serve.jsonl"
    opened = tmp_path / "opened.json"
    open_tool = Path(os.environ["PATH"]) / "open"
    open_tool.write_text(
        f"#!{sys.executable}\nimport json, pathlib, sys\n"
        f"pathlib.Path({str(opened)!r}).write_text(json.dumps(sys.argv[1:]))\n"
    )
    open_tool.chmod(0o755)
    return p, record, opened


def fake_server(ui, mode="normal", version="opencode v2.2.7"):
    p, record, _ = ui
    Path(p.settings.opencode_bin).write_text(
        f"#!{sys.executable}\n" + '''
import base64, http.server, json, os, pathlib, signal, sys, time
''' + f"if sys.argv[1:] == ['--version']: print({version!r}); sys.exit(0)\n" +
        f"record = pathlib.Path({str(record)!r})\nmode = {mode!r}\n" + '''
def save(data):
    with record.open('a') as f: f.write(json.dumps(data) + '\\n')
save({'argv': sys.argv[1:], 'cwd': os.getcwd(), 'pid': os.getpid(),
      'env': {k: os.environ.get(k) for k in ('HOME', 'XDG_DATA_HOME', 'OPENCODE_CONFIG')}})
if mode == 'exit': sys.exit(7)
if mode == 'tui':
    time.sleep(30)
    sys.exit(0)
password = os.environ['OPENCODE_PASSWORD']
# Exercise that even a verbose child cannot leak credentials through the launcher.
print(password, flush=True)
print('test-pair-code', file=sys.stderr, flush=True)
expected = 'Basic ' + base64.b64encode(('opencode:' + password).encode()).decode()
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        ok = self.headers.get('Authorization') == expected
        save({'method': 'GET', 'path': self.path, 'authenticated': ok})
        self.send_response(200 if ok and self.path == '/api/info' and mode != 'timeout' else 401)
        self.end_headers()
    def do_POST(self):
        ok = self.headers.get('Authorization') == expected
        save({'method': 'POST', 'path': self.path, 'authenticated': ok})
        self.send_response(200 if ok and self.path == '/api/pair' else 401)
        self.end_headers()
        self.wfile.write(json.dumps({'code': None if mode == 'bad-pair' else 'test-pair-code'}).encode())
port = int(sys.argv[sys.argv.index('--port') + 1])
http.server.HTTPServer(('127.0.0.1', port), Handler).serve_forever()
''')


def start_launcher(p, *extra, setup=""):
    # A fixed test password lets the parent check every captured byte for leakage.
    script = setup + "\n" + (
        "import secrets; secrets.token_urlsafe = lambda *args: 'ephemeral-test-password'; "
        "from assistant.cli import main; import sys; sys.exit(main(sys.argv[1:]))"
    )
    return subprocess.Popen(
        [sys.executable, "-c", script, "launch", "--profile", p.name,
         "--state-root", str(p.root.parent.parent), *extra],
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )


def wait_for(predicate, child):
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline:
        if predicate():
            return
        if child.poll() is not None:
            pytest.fail(f"launcher exited early: {child.communicate()}")
        time.sleep(0.02)
    pytest.fail("launcher did not become ready")


def read_ready(child):
    with selectors.DefaultSelector() as selector:
        selector.register(child.stdout, selectors.EVENT_READ)
        assert selector.select(timeout=5), "launcher did not print its ready message"
        return child.stdout.readline()


@pytest.mark.parametrize("stop_signal", [signal.SIGINT, signal.SIGTERM, signal.SIGHUP])
def test_launch_pairs_and_writes_pid(ui, stop_signal):
    p, record, opened = ui
    fake_server(ui)
    # Launch must pick up an updated harness rather than reuse this stale render.
    render_workspace(p, p.root / "harness-live")
    (p.root / "harness-live/LongTermMEMORY.md").write_text("Promoted memory fixture.\n")
    child = start_launcher(p)
    try:
        wait_for(lambda: opened.exists(), child)
        events = [json.loads(line) for line in record.read_text().splitlines()]
        serve_pid = events[0]["pid"]
        assert int((p.root / "ui.pid").read_text()) == serve_pid
        port = int(events[0]["argv"][events[0]["argv"].index("--port") + 1])
        assert json.loads((p.root / "ui.json").read_text()) == {"pid": serve_pid, "port": port}
        assert events[0]["argv"][:3] == ["serve", "--hostname", "127.0.0.1"]
        assert "--standalone" not in events[0]["argv"]
        assert events[0]["cwd"] == str(p.workspace)
        assert events[0]["env"]["XDG_DATA_HOME"] == str(p.opencode_home / "data")
        assert all(event["authenticated"] for event in events[1:])
        assert [(event["method"], event["path"]) for event in events[1:]] == [
            ("GET", "/api/info"), ("POST", "/api/pair"),
        ]
        url, = json.loads(opened.read_text())
        assert url.startswith("http://127.0.0.1:") and "/auth/connect/" in url
        assert "Promoted memory fixture." in (p.workspace / "AGENTS.md").read_text()
        ready = read_ready(child)
        child.send_signal(stop_signal)
        stdout, stderr = child.communicate(timeout=8)
        stdout = ready + stdout
        assert "Assistente aperto nel browser. Lascia aperta questa finestra; chiudila per spegnere l'assistente." in stdout
        assert PASSWORD not in stdout + stderr and PAIR_CODE not in stdout + stderr
        assert not (p.root / "ui.pid").exists()
        assert not (p.root / "ui.json").exists()
        with pytest.raises(ProcessLookupError):
            os.kill(serve_pid, 0)
        for log in p.logs_dir.glob("*"):
            assert PASSWORD not in log.read_text() and PAIR_CODE not in log.read_text()
    finally:
        if child.poll() is None:
            child.terminate()
            child.communicate(timeout=8)


def test_second_launch_reopens_running_ui_without_rendering(ui):
    p, record, opened = ui
    fake_server(ui)
    child = start_launcher(p)
    try:
        wait_for(opened.exists, child)
        read_ready(child)
        sentinel = p.workspace / "running-server-file"
        sentinel.write_text("keep workspace inode")
        inode = p.workspace.stat().st_ino
        events = record.read_bytes()
        first_url, = json.loads(opened.read_text())
        # Invalid harness ensures a second render would fail even before publication.
        (p.root / "harness-live/systemprompt.md").unlink()
        second = start_launcher(p)
        stdout, stderr = second.communicate(timeout=8)
        assert second.returncode == 0, stderr
        assert "L'assistente è già aperto nel browser." in stdout
        assert json.loads(opened.read_text()) == [first_url.split("/auth/")[0] + "/"]
        assert record.read_bytes() == events  # No additional server or pairing.
        assert p.workspace.stat().st_ino == inode
        assert sentinel.read_text() == "keep workspace inode"
        assert child.poll() is None
    finally:
        child.terminate()
        child.communicate(timeout=8)


def test_concurrent_launch_waits_for_metadata_and_holds_lock(ui):
    p, record, opened = ui
    fake_server(ui)
    blocked, release, second_started = (p.root / name for name in ("render-blocked", "release-render", "second-started"))
    setup = f'''
import importlib, pathlib, time
runtime = importlib.import_module('assistant.launch')
original_render = runtime.render_workspace
def delayed_render(*args, **kwargs):
    pathlib.Path({str(blocked)!r}).touch()
    deadline = time.monotonic() + 8
    while not pathlib.Path({str(release)!r}).exists():
        if time.monotonic() >= deadline:
            raise RuntimeError('test render was never released')
        time.sleep(0.01)
    return original_render(*args, **kwargs)
runtime.render_workspace = delayed_render
'''
    child = start_launcher(p, setup=setup)
    second = None
    try:
        wait_for(blocked.exists, child)
        assert not (p.root / "ui.pid").exists()
        with (p.root / "ui.lock").open("a") as lock:
            with pytest.raises(BlockingIOError):
                fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        second = start_launcher(p, setup=f"from pathlib import Path; Path({str(second_started)!r}).touch()")
        wait_for(second_started.exists, second)
        time.sleep(0.2)
        assert second.poll() is None
        assert not record.exists() and not (p.root / "ui.json").exists()
        release.touch()
        wait_for(opened.exists, child)
        read_ready(child)
        stdout, stderr = second.communicate(timeout=8)
        assert second.returncode == 0, stderr
        assert "L'assistente è già aperto nel browser." in stdout
        events = [json.loads(line) for line in record.read_text().splitlines()]
        assert len([event for event in events if "argv" in event]) == 1
        with (p.root / "ui.lock").open("a") as lock:
            with pytest.raises(BlockingIOError):
                fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        assert child.poll() is None
    finally:
        release.touch()
        for launcher in (second, child):
            if launcher is not None and launcher.poll() is None:
                launcher.terminate()
                launcher.communicate(timeout=8)
    with (p.root / "ui.lock").open("a") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)


def test_launch_lock_contention_reports_startup_after_five_seconds(ui):
    p, record, opened = ui
    fake_server(ui, "exit")
    render_workspace(p, p.root / "harness-live")
    sentinel = p.workspace / "keep-during-startup"
    sentinel.write_text("preserve workspace")
    inode = p.workspace.stat().st_ino
    with (p.root / "ui.lock").open("a") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        started = time.monotonic()
        child = start_launcher(p)
        try:
            stdout, stderr = child.communicate(timeout=8)
            elapsed = time.monotonic() - started
            assert child.returncode == 0, stderr
            assert "L'assistente si sta già aprendo." in stdout
            assert 5 <= elapsed < 8
            assert not record.exists() and not opened.exists()
            assert p.workspace.stat().st_ino == inode
            assert sentinel.read_text() == "preserve workspace"
            assert not (p.root / "ui.pid").exists()
        finally:
            if child.poll() is None:
                child.terminate()
                child.communicate(timeout=8)


@pytest.mark.parametrize("version, warns", [
    ("2.2.7", False), ("2.2.5", True), ("2.2.70", True),
    ("opencode v2.2.7", False), ("opencode v2.3.0", True),
])
def test_launch_version_warning_is_nonfatal_on_stderr(ui, version, warns):
    p, _, opened = ui
    fake_server(ui, version=version)
    child = start_launcher(p)
    try:
        wait_for(opened.exists, child)
        ready = read_ready(child)
        child.terminate()
        stdout, stderr = child.communicate(timeout=8)
        assert "Assistente aperto nel browser." in ready + stdout
        assert ("2.2.7" in stderr and "avviso" in stderr.lower()) == warns
        assert "avviso" not in stdout.lower()
    finally:
        if child.poll() is None:
            child.terminate()
            child.communicate(timeout=8)


def test_launch_server_exit_removes_pid(ui):
    p, record, opened = ui
    fake_server(ui)
    child = start_launcher(p)
    try:
        wait_for(opened.exists, child)
        read_ready(child)
        serve_pid = json.loads(record.read_text().splitlines()[0])["pid"]
        os.kill(serve_pid, signal.SIGTERM)
        child.communicate(timeout=8)
        assert child.returncode == 128 + signal.SIGTERM
        assert not (p.root / "ui.pid").exists()
    finally:
        if child.poll() is None:
            child.terminate()
            child.communicate(timeout=8)


def test_launch_open_failure_does_not_leak_pair_code(ui):
    p, _, opened = ui
    fake_server(ui)
    open_tool = Path(os.environ["PATH"]) / "open"
    open_tool.write_text(f"#!{sys.executable}\nimport sys\nprint(sys.argv[1])\nsys.exit(9)\n")
    child = start_launcher(p)
    try:
        stdout, stderr = child.communicate(timeout=8)
        assert child.returncode != 0 and "browser" in stderr
        assert PASSWORD not in stdout + stderr and PAIR_CODE not in stdout + stderr
        assert not opened.exists() and not (p.root / "ui.pid").exists()
    finally:
        if child.poll() is None:
            child.terminate()
            child.communicate(timeout=8)


@pytest.mark.parametrize("mode", ["exit", "bad-pair"])
def test_launch_failure_cleans_up(ui, mode):
    p, record, opened = ui
    fake_server(ui, mode)
    child = start_launcher(p)
    try:
        stdout, stderr = child.communicate(timeout=8)
        assert child.returncode != 0
        assert not opened.exists() and not (p.root / "ui.pid").exists()
        assert PASSWORD not in stdout + stderr and PAIR_CODE not in stdout + stderr
        serve_pid = json.loads(record.read_text().splitlines()[0])["pid"]
        with pytest.raises(ProcessLookupError):
            os.kill(serve_pid, 0)
    finally:
        if child.poll() is None:
            child.terminate()
            child.communicate(timeout=8)


def test_launch_readiness_timeout(ui):
    from assistant.launch import launch

    p, record, opened = ui
    fake_server(ui, "timeout")
    with pytest.raises(RuntimeError, match="(?i)(tempo|avvio|pronto)"):
        launch(p.name, state_root=p.root.parent.parent, timeout=0.3)
    assert not opened.exists() and not (p.root / "ui.pid").exists()
    assert record.exists()


def test_launch_cleans_pid_when_group_probe_is_denied(ui, monkeypatch, capsys):
    from assistant.launch import launch

    p, _, _ = ui
    fake_server(ui, "timeout")
    actual_killpg = os.killpg

    def restricted_killpg(pid, signum):
        if signum == 0:
            raise PermissionError("group observation denied")
        return actual_killpg(pid, signum)

    monkeypatch.setattr(os, "killpg", restricted_killpg)
    with pytest.raises(RuntimeError, match="Tempo di avvio"):
        launch(p.name, state_root=p.root.parent.parent, timeout=0.3)
    assert not (p.root / "ui.pid").exists()
    assert "verificare" in capsys.readouterr().err


def test_launch_tui_uses_ui_store_and_pid(ui):
    p, record, opened = ui
    fake_server(ui, "tui")
    child = start_launcher(p, "--tui")
    try:
        wait_for(lambda: record.exists() and (p.root / "ui.pid").exists(), child)
        invocation = json.loads(record.read_text().splitlines()[0])
        assert invocation["argv"] == []
        assert invocation["env"]["XDG_DATA_HOME"] == str(p.opencode_home / "data")
        assert int((p.root / "ui.pid").read_text()) == invocation["pid"]
        child.terminate()
        stdout, stderr = child.communicate(timeout=8)
        assert not (p.root / "ui.pid").exists() and not opened.exists(), (child.returncode, stdout, stderr)
    finally:
        if child.poll() is None:
            child.terminate()
            child.communicate(timeout=8)


def test_launch_snapshots_only_operator(ui, monkeypatch):
    from assistant.launch import launch

    p, _, _ = ui
    fake_server(ui, "exit")
    module = ModuleType("assistant.snapshot")
    calls = []
    module.take_snapshot = lambda p, now: calls.append((p.name, now))
    monkeypatch.setitem(sys.modules, "assistant.snapshot", module)
    with pytest.raises(RuntimeError):
        launch("dev", state_root=p.root.parent.parent)
    assert calls == []
    operator = load_profile("operator", state_root=p.root.parent.parent)
    import shutil

    shutil.copytree(p.root / "harness-live", operator.root / "harness-live")
    (operator.root / "settings.json").write_text((p.root / "settings.json").read_text())
    with pytest.raises(RuntimeError):
        launch("operator", state_root=p.root.parent.parent)
    assert calls[0][0] == "operator" and calls[0][1].utcoffset() is not None


def test_shutdown_kills_descendants_after_leader_exits(tmp_path):
    from assistant.launch import _terminate_group

    descendant = tmp_path / "descendant.pid"
    heartbeat = tmp_path / "heartbeat"
    script = f'''
import os, pathlib, signal, time
pid = os.fork()
if pid == 0:
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    pathlib.Path({str(descendant)!r}).write_text(str(os.getpid()))
    while True:
        with pathlib.Path({str(heartbeat)!r}).open('a') as f: f.write('.')
        time.sleep(0.01)
while True: time.sleep(1)
'''
    leader = subprocess.Popen([sys.executable, "-c", script], start_new_session=True)
    try:
        wait_for(descendant.exists, leader)
        _terminate_group(leader)
        time.sleep(0.05)
        size = heartbeat.stat().st_size
        time.sleep(0.1)
        assert heartbeat.stat().st_size == size, "descendant is still writing after shutdown"
    finally:
        try:
            os.killpg(leader.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        leader.wait(timeout=8)
