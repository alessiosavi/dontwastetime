"""Own the lifetime of a private, authenticated OpenCode UI process."""

import base64
from datetime import datetime, timezone
import importlib
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time
from urllib.error import URLError
from urllib.parse import quote
from urllib.request import ProxyHandler, Request, build_opener

from assistant.install import _atomic_write
from assistant.opencode_env import opencode_env
from assistant.profile import load_profile
from assistant.render import render_workspace


class _Stopped(Exception):
    def __init__(self, signum: int):
        self.signum = signum


def _stop_signal(signum, frame) -> None:
    raise _Stopped(signum)


def _snapshot_if_available(p) -> None:
    if p.name != "operator":
        return
    try:
        snapshot = importlib.import_module("assistant.snapshot")
    except ModuleNotFoundError as error:
        if error.name == "assistant.snapshot":
            return
        raise
    take_snapshot = getattr(snapshot, "take_snapshot", None)
    if take_snapshot is not None:
        take_snapshot(p, datetime.now(timezone.utc))


def _free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def _pair(process: subprocess.Popen, port: int, password: str, timeout: float) -> str:
    # Never consult parent proxy settings, even for this loopback-only request.
    client = build_opener(ProxyHandler({}))
    origin = f"http://127.0.0.1:{port}"
    authorization = "Basic " + base64.b64encode(f"opencode:{password}".encode()).decode("ascii")
    deadline = time.monotonic() + timeout
    while True:
        if process.poll() is not None:
            raise RuntimeError("OpenCode si è chiuso durante l'avvio. Riprova con --tui.")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError("Tempo di avvio OpenCode scaduto. Riprova con --tui.")
        try:
            request = Request(origin + "/api/info", headers={"Authorization": authorization})
            with client.open(request, timeout=min(0.5, remaining)) as response:
                if response.status == 200:
                    break
        except (URLError, OSError):
            pass
        time.sleep(min(0.1, max(0, deadline - time.monotonic())))
    if process.poll() is not None:
        raise RuntimeError("OpenCode si è chiuso durante l'avvio. Riprova con --tui.")
    try:
        request = Request(origin + "/api/pair", data=b"", headers={"Authorization": authorization})
        with client.open(request, timeout=max(0.01, deadline - time.monotonic())) as response:
            pair = json.load(response)
        code = pair.get("code") if isinstance(pair, dict) else None
        if not isinstance(code, str) or not code:
            raise ValueError
    except (URLError, OSError, ValueError):
        raise RuntimeError("Connessione privata a OpenCode non riuscita. Riprova con --tui.") from None
    return origin + "/auth/connect/" + quote(code, safe="")


def _terminate_group(process: subprocess.Popen) -> None:
    deadline = time.monotonic() + 5
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    while True:
        process.poll()  # Reap the leader; descendants may still own the group.
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            break
        except PermissionError:
            # Sandboxed macOS can deny a probe after the leader is reaped.
            # Report that uncertainty; do not attempt another group operation.
            print("Assistente: impossibile verificare l'arresto dell'intero gruppo OpenCode.", file=sys.stderr)
            process.wait(timeout=5)
            return
        if time.monotonic() >= deadline:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            break
        time.sleep(0.05)
    process.wait()


def launch(
    profile: str, *, state_root: Path | None = None, tui: bool = False, timeout: float = 20,
) -> int:
    """Re-render on every launch, pair in memory, then supervise until closed."""
    p = load_profile(profile, state_root)
    render_workspace(p, p.root / "harness-live")
    _snapshot_if_available(p)
    env = opencode_env(p, "ui", workspace=p.workspace)
    command = [p.settings.opencode_bin]
    if not tui:
        port = _free_port()
        password = secrets.token_urlsafe(32)
        env["OPENCODE_PASSWORD"] = password
        command += ["serve", "--hostname", "127.0.0.1", "--port", str(port)]
    previous = {}
    process = None
    pid_file = p.root / "ui.pid"
    try:
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            previous[signum] = signal.signal(signum, _stop_signal)
        process = subprocess.Popen(
            command, cwd=p.workspace, env=env, start_new_session=True,
            # The web server can log pairing credentials; discard its output.
            stdout=None if tui else subprocess.DEVNULL,
            stderr=None if tui else subprocess.DEVNULL,
        )
        _atomic_write(pid_file, f"{process.pid}\n")
        if not tui:
            url = _pair(process, port, password, timeout)
            result = subprocess.run(
                ["open", url], stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            if result.returncode:
                raise RuntimeError("Impossibile aprire il browser. Riprova con --tui.")
            print(
                "Assistente aperto nel browser. Lascia aperta questa finestra; "
                "chiudila per spegnere l'assistente.", flush=True,
            )
        returncode = process.wait()
        return returncode if returncode >= 0 else 128 - returncode
    except _Stopped as stopped:
        return 128 + stopped.signum
    finally:
        # A second close signal must not interrupt cleanup and orphan the server.
        for signum in previous:
            signal.signal(signum, signal.SIG_IGN)
        try:
            if process is not None:
                try:
                    _terminate_group(process)
                finally:
                    try:
                        if pid_file.read_text(encoding="utf-8").strip() == str(process.pid):
                            pid_file.unlink()
                    except FileNotFoundError:
                        pass
        finally:
            for signum, handler in previous.items():
                signal.signal(signum, handler)
