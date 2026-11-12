import json
from pathlib import Path
import subprocess

import pytest

from assistant.opencode_env import opencode_env, run_opencode


def test_env_never_points_at_developer_config(profile, tmp_path, monkeypatch):
    developer_home = tmp_path / "developer"
    monkeypatch.setenv("HOME", str(developer_home))
    for key in ("OPENCODE_CONFIG_CONTENT", "OPENCODE_DEVELOPER_OVERRIDE",
                "OPENCODE_DISABLE_PROJECT_CONFIG", "ANTHROPIC_API_KEY",
                "OPENAI_API_KEY", "PYTHONPATH"):
        monkeypatch.setenv(key, "parent-value")
    monkeypatch.setenv("OPENCODE_CONFIG_DIR", str(developer_home / ".config/opencode"))
    monkeypatch.setenv("XDG_DATA_HOME", str(developer_home / ".local/share"))
    monkeypatch.setenv("TMPDIR", str(developer_home / "tmp"))

    env = opencode_env(profile, "ui")

    assert all(str(developer_home) not in v for v in env.values())
    assert env["HOME"] == str(profile.opencode_home / "home")
    assert env["XDG_CONFIG_HOME"] == str(profile.opencode_home / "config")
    assert env["XDG_CACHE_HOME"] == str(profile.opencode_home / "cache")
    assert env["TMPDIR"] == str(profile.opencode_home / "tmp")
    assert env["OPENCODE_CONFIG_DIR"] == str(profile.opencode_home / "config-dir")
    assert env["OPENCODE_CONFIG"] == str(profile.workspace / "opencode.json")
    assert env["OPENCODE_DISABLE_CLAUDE_CODE"] == "true"
    assert env["NO_COLOR"] == "1"
    assert set(k for k in env if k.startswith("OPENCODE_")) == {
        "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG", "OPENCODE_DISABLE_CLAUDE_CODE",
    }
    assert "OPENCODE_DISABLE_PROJECT_CONFIG" not in env
    assert "ANTHROPIC_API_KEY" not in env
    assert "OPENAI_API_KEY" not in env
    assert "PYTHONPATH" not in env
    for key in ("HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
                "XDG_CACHE_HOME", "TMPDIR", "OPENCODE_CONFIG_DIR"):
        assert Path(env[key]).is_dir()
    assert list(Path(env["OPENCODE_CONFIG_DIR"]).iterdir()) == []


def test_env_parent_allowlist_and_workspace(profile, tmp_path):
    base = {key: "value" for key in (
        "PATH", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR",
    )}
    workspace = tmp_path / "rollout"
    env = opencode_env(profile, "ops", workspace=workspace, base={
        **base, "UNRELATED": "drop", "AZURE_API_KEY": "parent-fake-key",
        "OPENCODE_CONFIG_CONTENT": "drop",
    })
    assert all(env[key] == value for key, value in base.items())
    assert "UNRELATED" not in env
    assert "OPENCODE_CONFIG_CONTENT" not in env
    assert env["AZURE_API_KEY"] == "fake-file-key"
    assert env["OPENCODE_CONFIG"] == str(workspace / "opencode.json")
    assert workspace.is_dir()
    assert "LANG" not in opencode_env(profile, "ops", base={})


def test_env_loads_azure_key_from_secrets_file(profile, monkeypatch):
    monkeypatch.setenv("AZURE_API_KEY", "fake-parent-key")
    profile.settings.secrets_env.write_text(
        "# fake secrets\nUNRELATED=ignored\n\nAZURE_API_KEY = \"fake=file-key\"\n"
    )
    assert opencode_env(profile, "ui")["AZURE_API_KEY"] == "fake=file-key"
    profile.settings.secrets_env.write_text("OTHER_KEY=fake-other\n")
    with pytest.raises(ValueError, match="AZURE_API_KEY") as error:
        opencode_env(profile, "ui")
    assert "fake-other" not in str(error.value)
    profile.settings.secrets_env.write_text("AZURE_API_KEY=\"\"\n")
    with pytest.raises(ValueError, match="AZURE_API_KEY"):
        opencode_env(profile, "ui")
    profile.settings.secrets_env.unlink()
    with pytest.raises(FileNotFoundError, match="secrets"):
        opencode_env(profile, "ui")


def test_ui_and_ops_stores_differ(profile):
    ui = opencode_env(profile, "ui")
    ops = opencode_env(profile, "ops")
    assert ui["XDG_DATA_HOME"] == str(profile.root / "opencode/data")
    assert ui["XDG_STATE_HOME"] == str(profile.root / "opencode/state")
    assert ops["XDG_DATA_HOME"] == str(profile.root / "opencode-ops/data")
    assert ops["XDG_STATE_HOME"] == str(profile.root / "opencode-ops/state")
    for key in ("HOME", "XDG_CONFIG_HOME", "OPENCODE_CONFIG_DIR", "XDG_CACHE_HOME"):
        assert ui[key] == ops[key]


def test_run_opencode_uses_standalone_and_isolated_env(profile, fake_opencode, tmp_path):
    cwd = tmp_path / "replay-workspace"
    result = run_opencode(profile, "ops", ["run", "hello"], cwd=cwd, timeout=5,
                          extra_env={"EXPLICIT_OVERRIDE": "allowed"})
    invocation = fake_opencode.invocation()
    assert invocation["argv"].count("--standalone") == 1
    assert "hello" in invocation["argv"]
    assert invocation["cwd"] == str(cwd)
    assert invocation["stdin"] == ""
    assert invocation["env"]["HOME"] == str(profile.opencode_home / "home")
    assert invocation["env"]["AZURE_API_KEY"] == "fake-file-key"
    assert invocation["env"]["EXPLICIT_OVERRIDE"] == "allowed"
    assert invocation["env"]["OPENCODE_CONFIG"] == str(cwd / "opencode.json")
    assert result.returncode == 0
    assert json.loads(result.stdout) == invocation
    assert result.stderr == ""


@pytest.mark.parametrize(("args", "standalone"), [
    (["run", "hello"], True), (["session", "list"], True),
    (["export", "ses_fake"], True), (["api", "GET", "/api/skill"], True),
    (["debug", "config"], True), (["debug", "agents"], True),
    (["serve", "--hostname", "127.0.0.1"], False),
    (["debug", "paths"], False), (["--help"], False), (["--version"], False),
    (["run", "--help"], False), (["session", "--help"], False),
    (["run", "--standalone", "hello"], True),
])
def test_standalone_only_for_connecting_commands(profile, fake_opencode, args, standalone):
    original = args.copy()
    run_opencode(profile, "ui", args, cwd=profile.workspace, timeout=5)
    argv = fake_opencode.invocation()["argv"]
    assert argv.count("--standalone") == int(standalone)
    assert args == original


def test_run_opencode_requires_timeout(profile):
    with pytest.raises(TypeError, match="timeout"):
        run_opencode(profile, "ui", ["run", "hello"], cwd=profile.workspace)


def test_run_opencode_timeout_is_enforced(profile):
    with pytest.raises(subprocess.TimeoutExpired):
        run_opencode(profile, "ops", ["run", "--fake-sleep"],
                     cwd=profile.workspace, timeout=1)


def test_run_opencode_captures_failure(profile):
    result = run_opencode(profile, "ops", ["run", "--fake-fail"],
                          cwd=profile.workspace, timeout=5)
    assert result.returncode == 7
    assert result.stderr == "fake failure\n"
    assert isinstance(result.stdout, str)


def test_invalid_purpose_rejected(profile):
    with pytest.raises(ValueError, match="purpose"):
        opencode_env(profile, "invalid")


def test_standalone_flag_before_positional_separator(profile, fake_opencode):
    run_opencode(profile, "ui", ["run", "--", "--standalone"],
                 cwd=profile.workspace, timeout=5)
    argv = fake_opencode.invocation()["argv"]
    assert "--standalone" in argv[:argv.index("--")]
    assert argv[argv.index("--") + 1:] == ["--standalone"]


@pytest.mark.parametrize("timeout", [None, 0, -1])
def test_run_rejects_unbounded_or_invalid_timeout(profile, fake_opencode, timeout):
    with pytest.raises(ValueError, match="timeout"):
        run_opencode(profile, "ui", ["run", "hello"],
                     cwd=profile.workspace, timeout=timeout)
    assert not fake_opencode.record.exists()


@pytest.mark.parametrize("key", ["AZURE_API_KEY", "OPENCODE_DISABLE_PROJECT_CONFIG"])
def test_extra_env_preserves_key_source_and_project_discovery(profile, fake_opencode, key):
    with pytest.raises(ValueError, match=key):
        run_opencode(profile, "ui", ["run", "hello"], cwd=profile.workspace,
                     timeout=5, extra_env={key: "fake-override"})
    assert not fake_opencode.record.exists()
