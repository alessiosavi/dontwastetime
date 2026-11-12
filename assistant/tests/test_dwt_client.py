"""Exercise a real stdio MCP transport with synthetic servers and state."""

from dataclasses import replace
from datetime import datetime, timezone
import json
from pathlib import Path
import shutil
import sys

import pytest


REPO = Path(__file__).resolve().parents[2]


def test_dwt_client_tool_error_raises(profile, fake_dwt):
    from assistant.dwt_client import DwtClient, DwtError

    fake_dwt.path.write_text(
        f"#!{sys.executable}\n"
        "from mcp.server.fastmcp import FastMCP\n"
        "import os, sys\n"
        "server = FastMCP('fake-dwt')\n"
        "@server.tool()\n"
        "def context() -> dict[str, object]:\n"
        "    return {'argv':sys.argv[1:], 'pid':os.getpid()}\n"
        "@server.tool()\n"
        "def broken() -> dict[str, object]:\n"
        "    raise ValueError('synthetic tool failure')\n"
        "server.run(transport='stdio')\n"
    )
    now = datetime(2026, 11, 12, tzinfo=timezone.utc)
    state = profile.root / "replay"
    with DwtClient(profile, state_dir=state, now=now) as client:
        data = client.call("context")
        assert data["argv"] == ["mcp", "--operator", "operator", "--config",
                                str(profile.settings.dwt_config), "--env", "/dev/null",
                                "--state-dir", str(state), "--now", "2026-11-12T00:00:00+00:00"]
        with pytest.raises(DwtError, match="synthetic tool failure"):
            client.call("broken")
        assert client.call("context")["pid"] == data["pid"]
    import os
    with pytest.raises(ProcessLookupError):
        os.kill(data["pid"], 0)
    with pytest.raises((DwtError, RuntimeError)):
        client.call("context")


def test_dwt_client_rejects_naive_clock(profile):
    from assistant.dwt_client import DwtClient

    with pytest.raises(ValueError):
        DwtClient(profile, now=datetime(2026, 11, 12))


@pytest.mark.integration
def test_dwt_client_context_against_built_binary(profile):
    from assistant.dwt_client import DwtClient

    binary = REPO / "bin/dwt"
    assert binary.is_file(), "run make build first"
    config = profile.root / "test-config.json"
    config.write_text(json.dumps({
        "operators": ["operator"], "state_dir": str(profile.dwt_state_dir),
        "mailboxes": [{"address": "mail@example.test", "enabled": True}],
        "digest_recipients": ["operator@example.test"], "digest_sender": "mail@example.test",
    }))
    shutil.copyfile(REPO / "taxonomy.json", config.parent / "taxonomy.json")
    p = replace(profile, settings=replace(profile.settings, dwt_bin=str(binary), dwt_config=config))
    with DwtClient(p, now=datetime(2026, 3, 2, 23, 30, tzinfo=timezone.utc)) as client:
        context = client.call("context")
    assert context["operator"] == "operator"
    assert context["today"] == "2026-03-03"
    assert (profile.dwt_state_dir / "dwt.db").is_file()
