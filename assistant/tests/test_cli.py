"""CLI dispatch stays local; the notes server owns its stdio transport."""

from pathlib import Path
import subprocess
import sys

import pytest


def test_cli_help_lists_subcommands(capsys):
    from assistant.cli import main

    with pytest.raises(SystemExit) as result:
        main(["--help"])
    assert result.value.code == 0
    help_text = capsys.readouterr().out
    for command in ("install", "uninstall", "launch", "notes-mcp"):
        assert command in help_text
    result = subprocess.run([sys.executable, "-m", "assistant", "--help"], capture_output=True, text=True)
    assert result.returncode == 0
    assert "notes-mcp" in result.stdout and "launch" in result.stdout


def test_cli_passes_install_options(tmp_path, monkeypatch):
    from assistant import cli

    calls = []
    monkeypatch.setattr(cli, "install", lambda *args, **kwargs: calls.append((args, kwargs)))
    assert cli.main([
        "install", "--repo", str(tmp_path), "--profile", "dev", "--state-root", str(tmp_path / "state"),
        "--dev-recipient", "dev@example.com", "--provider-json", str(tmp_path / "provider.json"),
        "--desktop-launcher", "--no-launchd",
    ]) == 0
    assert calls == [(("dev",), {
        "repo": tmp_path, "state_root": tmp_path / "state", "dev_recipient": "dev@example.com",
        "provider_json": tmp_path / "provider.json", "desktop_launcher": True, "no_launchd": True,
    })]


def test_cli_uninstall_defaults(monkeypatch):
    from assistant import cli

    calls = []
    monkeypatch.setattr(cli, "uninstall", lambda *args, **kwargs: calls.append((args, kwargs)))
    assert cli.main(["uninstall", "--purge"]) == 0
    assert calls == [(("operator",), {"state_root": None, "purge": True})]


def test_cli_notes_mcp_dispatch(tmp_path, monkeypatch):
    from assistant import cli, notes_mcp

    calls = []
    server = type("Server", (), {"run": lambda self, **kwargs: calls.append(kwargs)})()
    monkeypatch.setattr(notes_mcp, "create_server", lambda memory_dir: calls.append(memory_dir) or server)
    assert cli.main(["notes-mcp", "--memory-dir", str(tmp_path)]) == 0
    assert calls == [tmp_path, {"transport": "stdio"}]


def test_tick_is_explicitly_unavailable(capsys):
    from assistant.cli import main

    assert main(["tick", "--profile", "dev", "--state-root", "/unused"]) != 0
    assert "tick non ancora disponibile" in capsys.readouterr().err


def test_makefile_assistant_targets(tmp_path):
    repo = Path(__file__).resolve().parents[2]
    result = subprocess.run(
        ["/usr/bin/make", "-n", "assistant-install", "PROFILE=dev", "DEV_RECIPIENT=dev@example.com"],
        cwd=repo, capture_output=True, text=True,
    )
    assert result.returncode == 0, result.stderr
    assert "uv run --project assistant python -m assistant install" in result.stdout
    assert "--profile dev" in result.stdout
    assert "--dev-recipient" in result.stdout and "dev@example.com" in result.stdout
    result = subprocess.run(
        ["/usr/bin/make", "-n", "assistant-uninstall", "PROFILE=dev", "PURGE=1"],
        cwd=repo, capture_output=True, text=True,
    )
    assert result.returncode == 0, result.stderr
    assert "uninstall" in result.stdout and "--purge" in result.stdout
