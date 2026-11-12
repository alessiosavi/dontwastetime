"""The executable bun replacement, its isolation policy and cost accounting."""

from dataclasses import replace
import fnmatch
import json
import os
from pathlib import Path
import subprocess

import pytest


PROMPT = "---\nRispondi solo: ok\n--model è un dato"


def bun_args(root, cwd, model, prompt=PROMPT):
    return ["run", "--cwd", str(root / "dependencies/opencode/packages/opencode"),
            "--conditions=browser", "src/index.ts", "run", prompt,
            "--dir", str(cwd), "--model", model, "--format", "json", "--variant", "high"]


def invoke(profile, tmp_path, *, prompt=PROMPT, extra=()):
    from assistant.bench.shim import prepare_harness2_root

    root = prepare_harness2_root(profile)
    cwd = tmp_path / "proposer"
    cwd.mkdir(exist_ok=True)
    env = dict(os.environ, XDG_DATA_HOME=str(cwd / ".ocdata"), XDG_STATE_HOME=str(cwd / ".ocstate"))
    result = subprocess.run(
        [str(root / "dependencies/tools/node_modules/.bin/bun"),
         *bun_args(root, cwd, profile.settings.model, prompt), *extra],
        cwd=cwd, env=env, capture_output=True, timeout=20,
    )
    return root, cwd, result


def test_shim_translates_bun_argv(profile, fake_opencode, tmp_path):
    root, cwd, result = invoke(profile, tmp_path, extra=("--title", "discarded"))
    assert result.returncode == 0, result.stderr
    call = fake_opencode.invocation()
    assert call["argv"] == ["run", "--standalone", "--format", "json", "--model",
                            profile.settings.model, "--", PROMPT]
    assert call["cwd"] == str(cwd)
    assert call["stdin"] == ""
    assert not (root / ".env").exists()


def test_shim_keeps_harness2_xdg_data(profile, fake_opencode, tmp_path):
    _, cwd, result = invoke(profile, tmp_path)
    assert result.returncode == 0, result.stderr
    env = fake_opencode.invocation()["env"]
    assert env["XDG_DATA_HOME"] == str(cwd / ".ocdata")
    assert env["XDG_STATE_HOME"] == str(cwd / ".ocstate")


def test_shim_uses_improver_profile(profile, fake_opencode, tmp_path, monkeypatch):
    for key in ("GRAPH_CLIENT_SECRET", "AZURE_API_KEY", "OPENCODE_PERMISSION", "OPENCODE_CONFIG_CONTENT",
                "OPENCODE_URL", "OPENCODE_PASSWORD", "OPENCODE_DISABLE_PROJECT_CONFIG", "ANTHROPIC_API_KEY"):
        monkeypatch.setenv(key, "parent-must-not-leak")
    root, _, result = invoke(profile, tmp_path)
    assert result.returncode == 0, result.stderr
    env = fake_opencode.invocation()["env"]
    assert env["AZURE_API_KEY"] == "fake-file-key"
    assert env["OPENCODE_DISABLE_CLAUDE_CODE"] == "true"
    assert env["NO_COLOR"] == "1"
    for key in ("HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMPDIR", "OPENCODE_CONFIG_DIR"):
        assert Path(env[key]).is_relative_to(root / "improver")
    assert not list(Path(env["OPENCODE_CONFIG_DIR"]).iterdir())
    for key in ("GRAPH_CLIENT_SECRET", "OPENCODE_PERMISSION", "OPENCODE_CONFIG_CONTENT", "OPENCODE_URL",
                "OPENCODE_PASSWORD", "OPENCODE_DISABLE_PROJECT_CONFIG", "ANTHROPIC_API_KEY"):
        assert key not in env
    assert env["OPENCODE_CONFIG"] == str(root / "improver/opencode.json")
    config = json.loads(Path(env["OPENCODE_CONFIG"]).read_text())
    assert "mcp" not in config and "instructions" not in config
    assert "fake-file-key" not in (root / "shim.json").read_text()


def test_improver_config_denies_shell_web_mcp_and_config_edits(profile):
    from assistant.bench.shim import prepare_harness2_root
    from assistant.render import opencode_config

    profile = replace(profile, settings=replace(profile.settings,
        provider={"models": {"DeepSeek-V4.1-Flash-1017": {"variants": {"max": {}}}}},
        model_cost={"input": 1, "output": 2, "cache_read": 0.1}))
    root = prepare_harness2_root(profile)
    config = json.loads((root / "improver/opencode.json").read_text())
    live = opencode_config(profile, profile.workspace)
    assert config["providers"] == live["providers"]
    assert config["model"] == live["model"]
    assert config["permissions"][0] == {"action": "*", "resource": "*", "effect": "deny"}
    assert config["permissions"][1:7] == [
        {"action": action, "resource": "*", "effect": "allow"}
        for action in ("read", "glob", "grep", "list", "edit")
    ] + [{"action": "edit", "resource": "opencode.json", "effect": "deny"}]

    # Q3: config rules follow built-ins; the last matching rule wins.
    def permission(action, resource):
        return [rule["effect"] for rule in config["permissions"]
                if fnmatch.fnmatchcase(action, rule["action"])
                and fnmatch.fnmatchcase(resource, rule["resource"])][-1]

    for action in ("shell", "bash", "webfetch", "websearch", "task", "subagent", "skill",
                   "dwt_context", "notes_remember", "external_directory"):
        assert permission(action, "*") == "deny"
    for action in ("read", "glob", "grep", "list", "edit"):
        assert permission(action, "context/harness/systemprompt.md") == "allow"
    for file in ("opencode.json", "opencode.jsonc", ".opencode/agent/x.md", ".opencode/opencode.json"):
        assert permission("edit", file) == "deny"


@pytest.mark.parametrize("reported_cost, expected_cost", [(0, 0.000045), (0.25, 0.5), (None, None)])
def test_shim_records_usage(profile, tmp_path, event_runner, reported_cost, expected_cost):
    profile = replace(profile, settings=replace(profile.settings,
        model_cost=None if reported_cost is None else {"input": 1, "output": 2, "cache_read": 0.5}))
    events = [{"type": "step_finish", "sessionID": "ses_improver", "part": {
        "id": str(i), "type": "step-finish", "cost": reported_cost or 0,
        "tokens": {"input": 10, "output": 5, "cache": {"read": 5}}}} for i in range(2)]
    stdout = b"banner\r\n" + b"".join(json.dumps(event).encode() + b"\n" for event in events)
    # Repeated step ids and repeated session ids must not double count.
    stdout += json.dumps(events[-1]).encode() + b"\n" + b"non-json:\xff"
    event_runner(stdout=stdout.decode("latin-1"), exit_code=7)
    _, _, result = invoke(profile, tmp_path)
    assert result.returncode == 7
    assert result.stdout == stdout.decode("latin-1").encode()
    _, _, again = invoke(profile, tmp_path)
    assert again.returncode == 7
    usage, = [json.loads(line) for line in (profile.root / "usage.jsonl").read_text().splitlines()]
    assert usage["call_id"] == "improver:ses_improver"
    assert (usage["tokens_in"], usage["tokens_out"], usage["tokens_cache_read"]) == (20, 10, 10)
    assert usage["model"] == profile.settings.model
    assert usage["cost_usd"] == pytest.approx(expected_cost) if expected_cost is not None else usage["cost_usd"] is None


def test_shim_relays_raw_bytes(profile, fake_opencode, tmp_path):
    import sys

    fake_opencode.path.write_text(f"#!{sys.executable}\nimport sys\n"
        "sys.stdout.buffer.write(b'raw\\xff\\r\\n')\n"
        "sys.stderr.buffer.write(b'error\\xfe\\r\\n')\nsys.exit(9)\n")
    _, _, result = invoke(profile, tmp_path)
    assert (result.returncode, result.stdout, result.stderr) == (9, b"raw\xff\r\n", b"error\xfe\r\n")


def test_shim_accounting_failure_preserves_child_exit(profile, tmp_path, event_runner):
    event_runner(exit_code=7)
    (profile.root / "usage.jsonl").write_text("invalid ledger\n")
    _, _, result = invoke(profile, tmp_path)
    assert result.returncode == 7
    assert b"usage" in result.stderr


def test_shim_refuses_oversized_prompt(profile, tmp_path, fake_opencode):
    # Test the validator directly: OS argv limits differ by platform.
    from assistant.bench.shim import _translate

    with pytest.raises(ValueError, match="200.000"):
        _translate(bun_args(tmp_path, tmp_path, profile.settings.model, "x" * 200_001),
                   {"model": profile.settings.model})
    assert not fake_opencode.record.exists()


@pytest.mark.parametrize("argv", [[], ["run"], ["run", "bad", "src/index.ts", "run", "P"]])
def test_shim_rejects_unknown_argv(profile, tmp_path, fake_opencode, argv):
    from assistant.bench.shim import prepare_harness2_root

    root = prepare_harness2_root(profile)
    result = subprocess.run([str(root / "dependencies/tools/node_modules/.bin/bun"), *argv],
                            capture_output=True, timeout=20)
    assert result.returncode == 2
    assert b"argv" in result.stderr
    assert not fake_opencode.record.exists()
