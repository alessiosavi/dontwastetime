"""Local executables and profiles; unit tests never invoke real services."""

from dataclasses import dataclass
import json
from pathlib import Path
import sys

import pytest


@dataclass(frozen=True)
class FakeExecutable:
    path: Path
    record: Path

    def invocation(self):
        return json.loads(self.record.read_text())


@pytest.fixture(autouse=True)
def fake_tools(tmp_path, monkeypatch):
    home = tmp_path / "home"
    home.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("DWT_CONFIG", str(tmp_path / "live-config.json"))
    monkeypatch.chdir(tmp_path)
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    tools = {}
    for name in ("opencode", "dwt"):
        path = bin_dir / name
        record = tmp_path / f"{name}-invocation.json"
        path.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, sys, time\n"
            "data = {'argv': sys.argv[1:], 'env': dict(os.environ), "
            "'cwd': os.getcwd(), 'stdin': sys.stdin.read()}\n"
            f"pathlib.Path({str(record)!r}).write_text(json.dumps(data))\n"
            "if '--fake-sleep' in sys.argv: time.sleep(30)\n"
            "print(json.dumps(data))\n"
            "if '--fake-fail' in sys.argv:\n"
            "    print('fake failure', file=sys.stderr)\n"
            "    sys.exit(7)\n"
        )
        path.chmod(0o700)
        tools[name] = FakeExecutable(path, record)
    monkeypatch.setenv("PATH", str(bin_dir))
    return tools


@pytest.fixture
def fake_opencode(fake_tools):
    return fake_tools["opencode"]


@pytest.fixture
def fake_dwt(fake_tools):
    return fake_tools["dwt"]


@pytest.fixture
def profile(tmp_path):
    from assistant.profile import load_profile

    p = load_profile("dev", state_root=tmp_path / "state")
    p.settings.secrets_env.write_text("AZURE_API_KEY=fake-file-key\n")
    return p
