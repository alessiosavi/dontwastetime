"""Installation contracts: all external tools and HOME belong to this test."""

import hashlib
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import plistlib
import shlex
import shutil
import sqlite3
import stat
import sys

import pytest

from assistant.profile import load_profile


MODEL = "DeepSeek-V4.1-Flash-1017"
HARNESS = Path(__file__).resolve().parents[1] / "harness"


@pytest.fixture
def installation(tmp_path, monkeypatch, fake_tools):
    repo = tmp_path / "repo with spaces"
    repo.mkdir()
    (repo / "bin").mkdir()
    (repo / "bin/dwt").write_text("fake dwt\n")
    (repo / "assistant").mkdir()
    shutil.copytree(HARNESS, repo / "assistant/harness")
    live = tmp_path / "live state"
    live.mkdir()
    with sqlite3.connect(live / "dwt.db") as db:
        db.execute("create table fixture (value text)")
        db.execute("insert into fixture values ('live mail')")
    config = {
        "state_dir": str(live), "digest_recipients": ["operator@example.com"],
        "alert_recipients": ["alerts@example.com"], "digest_sender": "admin@example.com",
        "poll_seconds": 180, "mailboxes": [{"address": "mail@example.com"}],
    }
    (repo / "config.json").write_text(json.dumps(config))
    (repo / "taxonomy.json").write_text('{"fixture": true}\n')
    provider = {
        "name": "Azure fixture", "package": "@ai-sdk/openai-compatible",
        "settings": {"baseURL": "https://example.invalid/v1", "apiKey": "{env:AZURE_API_KEY}"},
        "models": {MODEL: {"name": "Fixture", "cost": {"input": 1}}, "unused": {}},
    }
    provider_file = tmp_path / "provider.json"
    provider_file.write_text(json.dumps(provider))
    records = tmp_path / "tool-calls.jsonl"
    for name in ("uv", "launchctl", "open"):
        tool = Path(os.environ["PATH"]) / name
        tool.write_text(
            f"#!{sys.executable}\n"
            "import json, pathlib, sys\n"
            f"with pathlib.Path({str(records)!r}).open('a') as f:\n"
            f"    f.write(json.dumps([{name!r}, *sys.argv[1:]]) + '\\n')\n"
            "if sys.argv[1:2] == ['bootout']: sys.exit(3)\n"
        )
        tool.chmod(0o755)
    monkeypatch.setenv("AZURE_API_KEY", "installation-fixture-secret")
    return {
        "repo": repo, "live": live, "config": config, "provider": provider,
        "provider_file": provider_file, "state_root": tmp_path / "assistant state",
        "records": records,
    }


def do_install(installation, profile="dev", **kwargs):
    from assistant.install import install

    options = dict(
        repo=installation["repo"], state_root=installation["state_root"],
        provider_json=installation["provider_file"],
    )
    if profile == "dev":
        options["dev_recipient"] = "dev@example.com"
    options.update(kwargs)
    return install(profile, **options)


def calls(installation):
    return [json.loads(line) for line in installation["records"].read_text().splitlines()]


def test_plist_has_tick_schedule(tmp_path):
    from assistant.install import launchd_plist, launcher_script

    p = load_profile("dev", state_root=tmp_path / "state with spaces")
    before = set(p.root.iterdir())
    plist = plistlib.loads(launchd_plist(p).encode())
    assert plist == {
        "Label": "com.local.dontwastetime.assistant.dev",
        "ProgramArguments": [str(p.root / "venv/bin/python"), "-m", "assistant", "tick",
                             "--profile", "dev", "--state-root", str(tmp_path / "state with spaces")],
        "WorkingDirectory": str(p.root), "RunAtLoad": True, "StartInterval": 900,
        "StartCalendarInterval": {"Hour": 9, "Minute": 0}, "ProcessType": "Background",
        "StandardOutPath": str(p.logs_dir / "tick.log"),
        "StandardErrorPath": str(p.logs_dir / "tick.log"),
        "EnvironmentVariables": {"PATH": "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"},
    }
    script = launcher_script(p)
    assert script.startswith("#!/bin/bash\n")
    assert shlex.split(script.splitlines()[-1]) == [
        "exec", str(p.root / "venv/bin/python"), "-m", "assistant", "launch",
        "--profile", "dev", "--state-root", str(tmp_path / "state with spaces"),
    ]
    assert set(p.root.iterdir()) == before


@pytest.mark.parametrize("recipient", [None, "", "invalid", "Name <dev@example.com>", "a\nb@example.com"])
def test_dev_install_requires_recipient(installation, recipient):
    with pytest.raises(ValueError, match="(?i)(email|indirizzo|destinatario)"):
        do_install(installation, dev_recipient=recipient)
    assert not installation["records"].exists()


def test_dev_config_copy_only_mails_developer(installation):
    p = do_install(installation)
    config = json.loads(p.settings.dwt_config.read_text())
    assert config == {
        **installation["config"], "state_dir": str(p.root / "dwt"),
        "digest_recipients": ["dev@example.com"], "alert_recipients": ["dev@example.com"],
    }
    assert (p.root / "dwt/taxonomy.json").read_bytes() == (installation["repo"] / "taxonomy.json").read_bytes()
    assert (p.root / "dwt/dwt.db").is_file()
    assert p.settings.dwt_env == installation["repo"] / ".env"
    assert p.settings.dwt_bin == str(installation["repo"] / "bin/dwt")
    assert Path(p.settings.opencode_bin).is_absolute()
    assert (p.workspace / "AGENTS.md").is_file()
    assert (p.root / "harness-live/guardrails.md").is_file()
    tool_calls = calls(installation)
    assert tool_calls[:2] == [
        ["uv", "venv", "--python", "3.13", str(p.root / "venv")],
        ["uv", "pip", "install", "--python", str(p.root / "venv/bin/python"), str(installation["repo"] / "assistant")],
    ]
    label = "com.local.dontwastetime.assistant.dev"
    plist = Path.home() / f"Library/LaunchAgents/{label}.plist"
    assert plistlib.loads(plist.read_bytes())["Label"] == label
    assert tool_calls[2:] == [
        ["launchctl", "bootout", f"gui/{os.getuid()}/{label}"],
        ["launchctl", "bootstrap", f"gui/{os.getuid()}", str(plist)],
    ]
    assert stat.S_IMODE((p.root / "Assistente.command").stat().st_mode) == 0o755
    assert not (Path.home() / "Desktop/Assistente.command").exists()


def test_install_is_idempotent(installation):
    p = do_install(installation)
    (p.memory_dir / "notes.md").write_text("keep my note\n")
    settings_file = p.root / "settings.json"
    settings = json.loads(settings_file.read_text())
    settings.update(improve_every_hours=48, soft_cap_usd=12,
                    model_cost={"input": 1, "output": 2, "cache_read": 0.1})
    settings_file.write_text(json.dumps(settings))
    (p.root / "harness-live/stale.md").write_text("obsolete")
    installation["provider_file"].unlink()
    p2 = do_install(installation, provider_json=None)
    assert p2.root == p.root
    assert p2.settings.improve_every_hours == 48 and p2.settings.soft_cap_usd == 12
    assert p2.settings.model_cost == settings["model_cost"]
    assert p2.settings.provider == settings["provider"]
    assert (p.memory_dir / "notes.md").read_text() == "keep my note\n"
    assert not (p.root / "harness-live/stale.md").exists()
    assert [c[1] for c in calls(installation) if c[0] == "uv"] == ["venv", "pip", "venv", "pip"]


@pytest.mark.parametrize("profile_name", ["dev", "operator"])
def test_install_writes_installed_at(installation, profile_name):
    from assistant.markers import load_markers, save_markers

    before = datetime.now(timezone.utc)
    p = do_install(installation, profile_name, no_launchd=True)
    after = datetime.now(timezone.utc)
    markers = load_markers(p)
    assert markers.installed_at is not None
    assert before <= markers.installed_at <= after
    assert markers.installed_at.tzinfo == timezone.utc
    installed_at = markers.installed_at
    markers.last_prune = datetime(2026, 11, 11, tzinfo=timezone.utc)
    save_markers(p, markers)
    do_install(installation, profile_name, no_launchd=True)
    reinstalled = load_markers(p)
    assert reinstalled.installed_at == installed_at
    assert reinstalled.last_prune == datetime(2026, 11, 11, tzinfo=timezone.utc)


def test_uninstall_removes_plist_and_launcher_keeps_state(installation):
    from assistant.install import uninstall

    p = do_install(installation, desktop_launcher=True)
    desktop = Path.home() / "Desktop/Assistente.command"
    assert desktop.read_text() == (p.root / "Assistente.command").read_text()
    own_dwt = Path.home() / "Library/LaunchAgents/com.local.dontwastetime.plist"
    own_dwt.write_text("keep dwt")
    uninstall("dev", state_root=installation["state_root"])
    assert not (p.root / "Assistente.command").exists()
    assert not desktop.exists()
    assert not (own_dwt.parent / "com.local.dontwastetime.assistant.dev.plist").exists()
    assert own_dwt.read_text() == "keep dwt"
    assert (p.root / "settings.json").exists() and p.memory_dir.exists()
    assert all(c[-1] != f"gui/{os.getuid()}/com.local.dontwastetime" for c in calls(installation))


def test_uninstall_purge_removes_root(installation):
    from assistant.install import uninstall

    p = do_install(installation, desktop_launcher=True)
    desktop = Path.home() / "Desktop/Assistente.command"
    desktop.write_text("someone else's launcher")
    uninstall("dev", state_root=installation["state_root"], purge=True)
    assert not p.root.exists()
    assert desktop.read_text() == "someone else's launcher"
    assert (installation["live"] / "dwt.db").exists()


def test_secrets_file_mode_0600(installation, capsys):
    p = do_install(installation)
    assert p.settings.secrets_env.read_text() == "AZURE_API_KEY=installation-fixture-secret\n"
    assert stat.S_IMODE(p.settings.secrets_env.stat().st_mode) == 0o600
    assert stat.S_IMODE((p.root / "settings.json").stat().st_mode) == 0o600
    assert "installation-fixture-secret" not in capsys.readouterr().out


def test_provider_extraction_from_jsonc(installation):
    config = Path.home() / ".config/opencode/opencode.jsonc"
    config.parent.mkdir(parents=True)
    # Comment-looking strings and commas inside strings must survive unchanged.
    config.write_text('''{
      // A line comment
      "providers": {"azure-ai": {
        "name": "http://example.invalid/*literal*/,}", /* block comment */
        "settings": {"apiKey": "{env:AZURE_API_KEY}",},
        "models": {"DeepSeek-V4.1-Flash-1017": {"name": "a\\\"//b",}, "unused": {},},
      },},
    }''')
    p = do_install(installation, provider_json=None)
    assert p.settings.provider == {
        "name": "http://example.invalid/*literal*/,}",
        "settings": {"apiKey": "{env:AZURE_API_KEY}"},
        "models": {MODEL: {"name": 'a"//b'}},
    }


@pytest.mark.parametrize("key", ["apiKey", "api_key", "token", "clientSecret", "password", "key"])
def test_provider_extraction_refuses_literal_key(installation, key, capsys):
    provider = installation["provider"]
    provider["models"]["unused"] = {"nested": [{key: "literal-credential-never-print"}]}
    installation["provider_file"].write_text(json.dumps(provider))
    with pytest.raises(ValueError) as error:
        do_install(installation)
    assert "literal-credential-never-print" not in str(error.value)
    assert "literal-credential-never-print" not in repr(capsys.readouterr())
    assert not installation["records"].exists()


def test_install_never_modifies_live_config_or_db(installation):
    files = [installation["repo"] / "config.json", installation["live"] / "dwt.db",
             installation["repo"] / "taxonomy.json"]
    before = [hashlib.sha256(file.read_bytes()).hexdigest() for file in files]
    do_install(installation)
    assert before == [hashlib.sha256(file.read_bytes()).hexdigest() for file in files]


def test_dev_backup_includes_live_wal(installation):
    # Keep a writer open so committed WAL contents have not been checkpointed.
    with sqlite3.connect(installation["live"] / "dwt.db") as live:
        live.execute("pragma journal_mode=wal")
        live.execute("insert into fixture values ('committed WAL mail')")
        live.commit()
        p = do_install(installation)
        with sqlite3.connect(p.root / "dwt/dwt.db") as copy:
            assert copy.execute("select value from fixture").fetchall() == [
                ("live mail",), ("committed WAL mail",),
            ]


def test_reinstall_refreshes_existing_dev_wal(installation):
    p = do_install(installation)
    with sqlite3.connect(p.root / "dwt/dwt.db") as dev:
        dev.execute("pragma journal_mode=wal")
        dev.execute("update fixture set value = 'old dev WAL'")
        dev.commit()
        do_install(installation)
        with sqlite3.connect(p.root / "dwt/dwt.db") as refreshed:
            assert refreshed.execute("select value from fixture").fetchall() == [("live mail",)]
        assert dev.execute("select value from fixture").fetchall() == [("live mail",)]


def test_install_rejects_dev_db_hardlink_to_live(installation):
    p = load_profile("dev", state_root=installation["state_root"])
    (p.root / "dwt").mkdir()
    os.link(installation["live"] / "dwt.db", p.root / "dwt/dwt.db")
    with pytest.raises(ValueError, match="(?i)(separat|live)"):
        do_install(installation)
    assert not installation["records"].exists()


def test_operator_uses_live_config_and_no_launchd(installation):
    p = do_install(installation, "operator", no_launchd=True)
    assert p.settings.dwt_config == installation["repo"] / "config.json"
    assert p.settings.dwt_env == installation["repo"] / ".env"
    assert not (p.root / "dwt").exists()
    assert all(c[0] != "launchctl" for c in calls(installation))
    assert not (Path.home() / "Library/LaunchAgents").exists()


@pytest.mark.parametrize("profile_name", ["dev", "operator"])
def test_install_stores_recap_recipient(installation, profile_name):
    p = do_install(installation, profile_name, no_launchd=True)
    expected = "dev@example.com" if profile_name == "dev" else "operator@example.com"
    assert p.settings.recap_recipient == expected
    assert load_profile(profile_name, installation["state_root"]).settings.recap_recipient == expected


@pytest.mark.parametrize("source", ["process", "env_file", "config", "default"])
@pytest.mark.parametrize("profile_name", ["dev", "operator"])
def test_install_resolves_and_stores_dwt_state(installation, monkeypatch, source, profile_name):
    repo = installation["repo"]
    live = (Path.home() / "Library/Application Support/dontwastetime" if source == "default"
            else installation["state_root"] / f"live-{source}")
    live.mkdir(parents=True)
    with sqlite3.connect(live / "dwt.db") as db:
        db.execute("create table source_marker (value text)")
        db.execute("insert into source_marker values (?)", (source,))
    config = installation["config"]
    if source == "default":
        config.pop("state_dir")
    else:
        config["state_dir"] = str(live if source == "config" else repo / "wrong-config-state")
    (repo / "config.json").write_text(json.dumps(config))
    if source in ("process", "env_file"):
        (repo / ".env").write_text(f'DWT_STATE_DIR="{live if source == "env_file" else repo / "wrong-env-state"}"\n')
    if source == "process":
        monkeypatch.setenv("DWT_STATE_DIR", str(live))
    p = do_install(installation, profile_name, no_launchd=True)
    expected = p.root / "dwt" if profile_name == "dev" else live
    assert p.settings.dwt_state_dir == str(expected)
    monkeypatch.setenv("DWT_STATE_DIR", str(repo / "changed-environment"))
    assert load_profile(profile_name, installation["state_root"]).dwt_state_dir == expected
    if profile_name == "dev":
        with sqlite3.connect(p.dwt_state_dir / "dwt.db") as db:
            assert db.execute("select value from source_marker").fetchall() == [(source,)]


def test_install_without_state_root_locates_profiles_in_live_state(installation):
    p = do_install(installation, "operator", state_root=None, no_launchd=True)
    assert p.root == installation["live"] / "assistant/operator"


@pytest.mark.parametrize("version, warns", [
    ("2.2.7", False), ("2.2.5", True), ("2.2.70", True),
    ("opencode v2.2.7", False), ("opencode v2.3.0", True),
])
def test_install_version_warning_is_nonfatal_on_stderr(installation, version, warns, capsys):
    tool = Path(os.environ["PATH"]) / "opencode"
    tool.write_text(f"#!{sys.executable}\nprint({version!r})\n")
    p = do_install(installation, no_launchd=True)
    assert (p.root / "settings.json").is_file()
    captured = capsys.readouterr()
    assert ("2.2.7" in captured.err and "avviso" in captured.err.lower()) == warns
    assert "avviso" not in captured.out.lower()


def test_operator_install_accepts_explicit_digest_recipient_case_insensitively(installation):
    config = {**installation["config"], "digest_recipients": ["operator@example.com", "other@example.com"]}
    (installation["repo"] / "config.json").write_text(json.dumps(config))
    p = do_install(installation, "operator", recap_recipient="person.two@example.com", no_launchd=True)
    assert p.settings.recap_recipient == "person.two@example.com"


@pytest.mark.parametrize("recipients, explicit, expected", [
    (["operator@example.com", "other@example.com"], None, "operator@example.com"),
    (["person.two@example.com", "other@example.com"], None, "operator@example.com"),
    (["new@example.com"], None, "new@example.com"),
    (["new@example.com", "other@example.com"], None, None),
    (["operator@example.com", "other@example.com"], "other@example.com", "other@example.com"),
])
def test_operator_reinstall_preserves_only_allowlisted_recap_recipient(installation, recipients, explicit, expected):
    p = do_install(installation, "operator", no_launchd=True)
    config_file = installation["repo"] / "config.json"
    config_file.write_text(json.dumps({**installation["config"], "digest_recipients": recipients}))
    previous_calls = calls(installation)
    previous_settings = (p.root / "settings.json").read_bytes()
    if expected is None:
        with pytest.raises(ValueError, match="--recap-recipient"):
            do_install(installation, "operator", recap_recipient=explicit, no_launchd=True)
        assert calls(installation) == previous_calls
        assert (p.root / "settings.json").read_bytes() == previous_settings
    else:
        refreshed = do_install(installation, "operator", recap_recipient=explicit, no_launchd=True)
        assert refreshed.settings.recap_recipient == expected
        assert load_profile("operator", installation["state_root"]).settings.recap_recipient == expected


@pytest.mark.parametrize("profile_name", ["dev", "operator"])
def test_reinstall_refuses_active_ui_before_mutating_profile(installation, profile_name, capsys):
    from assistant.cli import main

    p = do_install(installation, profile_name, no_launchd=True)
    (p.root / "ui.pid").write_text(str(os.getpid()))
    before = {file.relative_to(p.root): (file.read_bytes(), file.stat().st_ino)
              for file in p.root.rglob("*") if file.is_file()}
    previous_calls = calls(installation)
    args = ["install", "--profile", profile_name, "--repo", str(installation["repo"]),
            "--state-root", str(installation["state_root"]), "--no-launchd"]
    if profile_name == "dev":
        args += ["--dev-recipient", "dev@example.com"]
    result = main(args)
    assert result != 0
    assert "Chiudi l'assistente prima di aggiornarlo." in capsys.readouterr().err
    assert calls(installation) == previous_calls
    assert {file.relative_to(p.root): (file.read_bytes(), file.stat().st_ino)
            for file in p.root.rglob("*") if file.is_file()} == before


@pytest.mark.parametrize("recipients, chosen", [
    ([], None), (["a@example.com", "b@example.com"], None),
    (["operator@example.com"], "other@example.com"), ([""], None),
])
def test_operator_install_rejects_missing_or_unauthorized_recipient(installation, recipients, chosen):
    (installation["repo"] / "config.json").write_text(json.dumps({"digest_recipients": recipients}))
    with pytest.raises(ValueError, match="--recap-recipient"):
        do_install(installation, "operator", recap_recipient=chosen)
    assert not installation["records"].exists()


@pytest.mark.parametrize("missing", ["key", "dwt", "opencode"])
def test_install_prerequisites_fail_before_tools(installation, monkeypatch, missing):
    if missing == "key":
        monkeypatch.delenv("AZURE_API_KEY")
    elif missing == "dwt":
        (installation["repo"] / "bin/dwt").unlink()
    else:
        (Path(os.environ["PATH"]) / "opencode").unlink()
    with pytest.raises((ValueError, FileNotFoundError), match={
        "key": "AZURE_API_KEY", "dwt": "make build", "opencode": "opencode",
    }[missing]):
        do_install(installation)
    assert not installation["records"].exists()


def test_launcher_quotes_shell_metacharacters(tmp_path):
    from assistant.install import launcher_script

    p = load_profile("operator", state_root=tmp_path / "state '$`with\" chars")
    assert shlex.split(launcher_script(p).splitlines()[-1])[-1] == str(p.root.parent.parent)


def test_promoted_status_recorded_on_install(installation):
    from assistant.bench.improve import fingerprint
    from assistant.status import ack_status, pending_status

    p = do_install(installation, no_launchd=True)
    old = fingerprint(p.root / "harness-live")
    candidate = p.bench_dir / "runs/promoted-run/candidate"
    shutil.copytree(installation["repo"] / "assistant/harness", candidate)
    path = candidate / "systemprompt.md"
    path.write_text(path.read_text() + "\nRisposte più brevi nelle bozze.\n")
    (candidate.parent / "GATE").write_text("passed\n")
    (candidate.parent / "report.md").write_text("## Sintesi\n\nHo imparato: risposte più brevi nelle bozze\n")
    shutil.copytree(candidate, installation["repo"] / "assistant/harness", dirs_exist_ok=True)
    do_install(installation, no_launchd=True)
    events = pending_status(p)
    assert fingerprint(p.root / "harness-live") != old
    assert [(event.key, event.text) for event in events] == [
        ("promoted:promoted-run", "Ho imparato: risposte più brevi nelle bozze")]
    ack_status(p, ["promoted:promoted-run"])
    do_install(installation, no_launchd=True)
    assert pending_status(p) == []
