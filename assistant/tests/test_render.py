"""Rendered workspace contracts; real model calls are opt-in only."""

from dataclasses import replace
from datetime import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
from types import SimpleNamespace

import pytest

from assistant.harness_rules import REQUIRED_GUARDRAIL_LINES


HARNESS_ROOT = Path(__file__).resolve().parents[1] / "harness"
REAL_PATH = os.environ.get("PATH", "")
REAL_OPENCODE = shutil.which("opencode")
READ_TOOLS = {
    "dwt_context", "dwt_overview", "dwt_needs_reply", "dwt_list_messages",
    "dwt_search", "dwt_get_thread", "dwt_get_message", "dwt_digest", "dwt_health",
}


def read_config(workspace):
    return json.loads((workspace / "opencode.json").read_text(encoding="utf-8"))


def permission(config, action):
    import fnmatch

    effect = "deny"
    for rule in config["permissions"]:
        if fnmatch.fnmatchcase(action, rule["action"]):
            effect = rule["effect"]
    return effect


def test_h2_import_does_not_load_cwd_dotenv(tmp_path):
    (tmp_path / ".env").write_text("H2_LEAK_PROBE=1\n")
    env = {k: v for k, v in os.environ.items() if k != "H2_LEAK_PROBE"}
    result = subprocess.run(
        [sys.executable, "-c", """
import os
from pathlib import Path
before = Path.cwd()
from assistant.h2 import harness2
h2 = harness2()
assert harness2() is h2
assert h2.OPENCODE_SURFACE.name == 'opencode'
assert h2.OpencodeSubstrate and h2.render_opencode_tree and h2.tree
assert Path.cwd() == before
assert 'H2_LEAK_PROBE' not in os.environ
from assistant.harness_rules import validate_tree
assert 'H2_LEAK_PROBE' not in os.environ
"""], cwd=tmp_path, env=env, capture_output=True, text=True, timeout=20,
    )
    assert result.returncode == 0, result.stderr


def test_render_writes_agents_md_and_skills(profile):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)

    assert workspace == profile.workspace
    assert REQUIRED_GUARDRAIL_LINES[0] in (workspace / "AGENTS.md").read_text()
    assert (workspace / ".opencode/skills/triage/SKILL.md").read_text() == (
        HARNESS_ROOT / "skills/triage/SKILL.md"
    ).read_text()
    config = read_config(workspace)
    assert config["instructions"] == [
        str(profile.memory_dir / "notes.md"),
        str(profile.memory_dir / "recap/latest.md"),
        str(profile.memory_dir / "status.md"),
    ]
    assert all(Path(path).is_absolute() and Path(path).is_file()
               for path in config["instructions"])


def test_config_live_profile(profile):
    from assistant.render import opencode_config

    config = opencode_config(profile, profile.workspace)

    assert config["$schema"] == "https://opencode.ai/config.json"
    assert config["model"] == profile.settings.model
    assert config["permissions"][0] == {"action": "*", "resource": "*", "effect": "deny"}
    assert [r["action"] for r in config["permissions"][1:]] == [
        "read", "skill", "dwt_*", "notes_*",
    ]
    for tool in ("read", "skill", "dwt_triage", "notes_remember"):
        assert permission(config, tool) == "allow"
    for tool in ("bash", "edit", "write", "patch", "webfetch", "websearch", "task",
                 "external_directory", "some_future_tool"):
        assert permission(config, tool) == "deny"


@pytest.mark.parametrize("cost", [None, {"input": 0.3, "output": 0.7, "cache_read": 0.03}])
def test_config_provider_key_and_model_cost(profile, cost):
    from assistant.render import opencode_config

    provider = {
        "name": "Azure fixture", "package": "@ai-sdk/openai-compatible",
        "settings": {"baseURL": "https://example.invalid/v1", "apiKey": "replace-me"},
        "models": {"DeepSeek-V4.1-Flash-1017": {"name": "Fixture", "limit": {"context": 1000}}},
    }
    before = json.dumps(provider)
    p = replace(profile, settings=replace(profile.settings, provider=provider, model_cost=cost))

    config = opencode_config(p, p.workspace)

    rendered = config["providers"]["azure-ai"]
    assert rendered["settings"] == {
        "baseURL": "https://example.invalid/v1", "apiKey": "{env:AZURE_API_KEY}",
    }
    model = rendered["models"]["DeepSeek-V4.1-Flash-1017"]
    assert model["limit"] == {"context": 1000}
    if cost is None:
        assert "cost" not in model
    else:
        assert model["cost"] == {"input": 0.3, "output": 0.7, "cache": {"read": 0.03}}
    assert json.dumps(provider) == before


def test_render_has_no_plugin_or_scripts(profile):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)

    assert not (workspace / ".opencode/plugin").exists()
    assert not list((workspace / ".opencode/agent").glob("*.md"))
    assert not list(workspace.rglob("scripts"))


def test_config_dwt_command_has_now_only_when_given(profile, tmp_path):
    from assistant.render import opencode_config

    config = opencode_config(profile, profile.workspace)
    command = [profile.settings.dwt_bin, "mcp", "--operator", "operator", "--config",
               str(profile.settings.dwt_config), "--env", "/dev/null", "--state-dir",
               str(profile.dwt_state_dir)]
    assert config["mcp"] == {"servers": {
        "dwt": {"type": "local", "command": command, "codemode": False, "protocol": "legacy"},
        "notes": {"type": "local", "command": [sys.executable, "-m", "assistant", "notes-mcp",
                                                  "--memory-dir", str(profile.memory_dir)],
                  "codemode": False, "protocol": "legacy"},
    }}
    now = datetime.fromisoformat("2026-11-12T09:00:00+01:00")
    config = opencode_config(profile, tmp_path / "replay", dwt_state_dir=tmp_path / "snapshot",
                             now=now, notes_dir=tmp_path / "replay-notes")
    assert config["mcp"]["servers"]["dwt"]["command"] == (
        command[:-1] + [str(tmp_path / "snapshot"), "--now", "2026-11-12T09:00:00+01:00"]
    )
    assert config["mcp"]["servers"]["notes"]["command"][-1] == str(tmp_path / "replay-notes")
    assert config["instructions"] == [str(tmp_path / "replay-notes" / path)
                                       for path in ("notes.md", "recap/latest.md", "status.md")]


def test_config_rejects_naive_now(profile):
    from assistant.render import opencode_config

    with pytest.raises(ValueError, match="timezone|aware"):
        opencode_config(profile, profile.workspace, now=datetime(2026, 11, 12, 9))


def test_rerender_with_disposable_notes_keeps_the_disposable_link(profile, tmp_path):
    from assistant.render import render_workspace

    notes = tmp_path / "disposable-notes"
    workspace = tmp_path / "replay"
    render_workspace(profile, HARNESS_ROOT, workspace, notes_dir=notes)
    (notes / "notes.md").write_text("replay note")
    render_workspace(profile, HARNESS_ROOT, workspace, notes_dir=notes)
    assert (workspace / "memory").resolve() == notes
    assert (workspace / "memory/notes.md").read_text() == "replay note"
    assert not profile.memory_dir.exists()


def test_rerender_keeps_memory(profile):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)
    notes = workspace / "memory/notes.jsonl"
    notes.write_text('{"text":"da conservare"}\n')
    (workspace / "memory/notes.md").write_text("Una nota di Operator.\n")
    for stale in ("obsolete.txt", ".opencode/plugin/stale.ts", "scripts/stale.py"):
        path = workspace / stale
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("old")

    render_workspace(profile, HARNESS_ROOT)

    assert (workspace / "memory").is_symlink()
    assert (workspace / "memory").resolve() == profile.memory_dir
    assert notes.read_text() == '{"text":"da conservare"}\n'
    assert (workspace / "memory/notes.md").read_text() == "Una nota di Operator.\n"
    assert not (workspace / "obsolete.txt").exists()
    assert not (workspace / ".opencode/plugin").exists()
    assert not (workspace / "scripts").exists()


def test_render_rejects_invalid_tree(profile, tmp_path):
    from assistant.render import render_workspace

    tree = tmp_path / "invalid-tree"
    shutil.copytree(HARNESS_ROOT, tree)
    guardrails = tree / "guardrails.md"
    guardrails.write_text(guardrails.read_text().replace(REQUIRED_GUARDRAIL_LINES[0], ""))
    dest = tmp_path / "untouched"

    with pytest.raises(ValueError, match="guardrails.md"):
        render_workspace(profile, tree, dest)

    assert not dest.exists()
    assert not profile.memory_dir.exists()


def test_always_on_including_notes_and_recap_under_cap(profile, tmp_path):
    from assistant.render import render_workspace

    tree = tmp_path / "tree"
    shutil.copytree(HARNESS_ROOT, tree)
    workspace = render_workspace(profile, tree)
    size = len((workspace / "AGENTS.md").read_text())
    memory = tree / "LongTermMEMORY.md"
    memory.write_text(memory.read_text().rstrip() + "è" * (16_000 - size))
    for name, cap in (("notes.md", 4000), ("recap/latest.md", 3000), ("status.md", 1000)):
        (profile.memory_dir / name).write_text("è" * cap)

    workspace = render_workspace(profile, tree)

    agents_size = len((workspace / "AGENTS.md").read_text())
    assert agents_size == 16_000
    assert agents_size + sum(len(Path(path).read_text())
                             for path in read_config(workspace)["instructions"]) == 24_000
    memory.write_text(memory.read_text() + "è")
    with pytest.raises(ValueError, match="16000|24000"):
        render_workspace(profile, tree)
    assert len((workspace / "AGENTS.md").read_text()) == 16_000


@pytest.mark.parametrize(("name", "cap"), [("notes.md", 4000), ("recap/latest.md", 3000),
                                           ("status.md", 1000)])
def test_instruction_file_caps_preserve_workspace(profile, name, cap):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)
    original = (workspace / "AGENTS.md").read_bytes()
    (workspace / "sentinel").write_text("keep")
    (profile.memory_dir / name).write_text("è" * (cap + 1))

    with pytest.raises(ValueError, match=str(cap)):
        render_workspace(profile, HARNESS_ROOT)

    assert (workspace / "AGENTS.md").read_bytes() == original
    assert (workspace / "sentinel").read_text() == "keep"


@pytest.mark.parametrize("forbidden", [None, ".opencode/plugin/evil.ts",
                                       ".opencode/agent/evil.md", "scripts/evil.py"])
def test_render_failure_leaves_workspace_intact(profile, monkeypatch, forbidden):
    from assistant import render

    workspace = render.render_workspace(profile, HARNESS_ROOT)
    original = (workspace / "AGENTS.md").read_bytes()
    (workspace / "sentinel").write_text("keep")
    h2 = render.harness2()
    temporary_paths = []

    class Substrate:
        def materialise(self, *args):
            install = h2.OpencodeSubstrate().materialise(*args)
            temporary_paths.append(install.staging_dir)
            return install

    def fail_or_inject(staged, dest, ctx):
        temporary_paths.append(dest)
        h2.render_opencode_tree(staged, dest, ctx)
        if forbidden is None:
            raise RuntimeError("renderer failed")
        path = dest / forbidden
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("unsafe")

    monkeypatch.setattr(render, "harness2", lambda: SimpleNamespace(
        OpencodeSubstrate=Substrate, render_opencode_tree=fail_or_inject,
    ))

    with pytest.raises((ValueError, RuntimeError)):
        render.render_workspace(profile, HARNESS_ROOT)

    assert (workspace / "AGENTS.md").read_bytes() == original
    assert (workspace / "sentinel").read_text() == "keep"
    assert (workspace / "memory").is_symlink()
    assert not any(path.exists() for path in temporary_paths)


def test_publish_rename_failure_restores_workspace(profile, monkeypatch):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)
    (workspace / "sentinel").write_text("keep")
    original = (workspace / "opencode.json").read_bytes()
    rename = Path.rename

    def fail_publish(path, target):
        if path.name == "workspace" and path.parent.name.startswith(".assistant-render-"):
            raise OSError("replacement failed")
        return rename(path, target)

    monkeypatch.setattr(Path, "rename", fail_publish)

    with pytest.raises(OSError, match="replacement failed"):
        render_workspace(profile, HARNESS_ROOT)

    assert (workspace / "sentinel").read_text() == "keep"
    assert (workspace / "opencode.json").read_bytes() == original
    assert (workspace / "memory").resolve() == profile.memory_dir
    assert not list(workspace.parent.glob(".assistant-render-*"))


def test_failed_rollback_preserves_recoverable_workspace(profile, monkeypatch):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)
    (workspace / "sentinel").write_text("keep")
    rename = Path.rename

    def fail_replacement_and_rollback(path, target):
        if path.parent.name.startswith(".assistant-render-"):
            raise OSError("filesystem refuses rename")
        return rename(path, target)

    monkeypatch.setattr(Path, "rename", fail_replacement_and_rollback)

    with pytest.raises(RuntimeError, match="preserved"):
        render_workspace(profile, HARNESS_ROOT)

    backups = list(workspace.parent.glob(".assistant-render-*/previous"))
    assert len(backups) == 1
    assert (backups[0] / "sentinel").read_text() == "keep"
    assert (backups[0] / "memory").resolve() == profile.memory_dir


def test_symlinked_skill_directory_cannot_bypass_validation(profile, tmp_path):
    from assistant.render import render_workspace

    tree = tmp_path / "tree"
    shutil.copytree(HARNESS_ROOT, tree)
    external = tmp_path / "external"
    external.mkdir()
    (external / "unvalidated.py").write_text("print('not Markdown')")
    (tree / "skills/triage/helpers").symlink_to(external, target_is_directory=True)
    workspace = render_workspace(profile, HARNESS_ROOT)
    (workspace / "sentinel").write_text("keep")

    with pytest.raises(ValueError, match="unvalidated.py|symlink"):
        render_workspace(profile, tree)

    assert (workspace / "sentinel").read_text() == "keep"
    assert not (workspace / ".opencode/skills/triage/helpers").exists()


@pytest.mark.parametrize("relative", [".", "memory", "memory/nested"])
def test_render_refuses_destination_overlapping_memory(profile, relative):
    from assistant.render import render_workspace

    profile.memory_dir.mkdir()
    sentinel = profile.memory_dir / "notes.jsonl"
    sentinel.write_text("keep")

    with pytest.raises(ValueError, match="overlap"):
        render_workspace(profile, HARNESS_ROOT, profile.root / relative)

    assert sentinel.read_text() == "keep"


def test_render_rejects_instruction_files_inside_replaced_workspace(profile):
    from assistant.render import render_workspace

    workspace = render_workspace(profile, HARNESS_ROOT)
    (workspace / "sentinel").write_text("keep")

    with pytest.raises(ValueError, match="instruction.*overlap"):
        render_workspace(profile, HARNESS_ROOT, notes_dir=workspace / "replay-notes")

    assert (workspace / "sentinel").read_text() == "keep"
    assert not (workspace / "replay-notes").exists()


@pytest.mark.parametrize("dwt_read_tools", [False, True])
def test_ops_workspace_permissions(profile, dwt_read_tools):
    from assistant.render import ops_workspace

    workspace = ops_workspace(profile, dwt_read_tools=dwt_read_tools)
    config = read_config(workspace)

    assert workspace.parent == profile.root / "ops-workspaces"
    assert {path.name for path in workspace.iterdir()} == {"opencode.json"}
    assert config["permissions"][0] == {"action": "*", "resource": "*", "effect": "deny"}
    assert {r["action"] for r in config["permissions"][1:]} == (READ_TOOLS if dwt_read_tools else set())
    assert set(config.get("mcp", {}).get("servers", {})) == ({"dwt"} if dwt_read_tools else set())
    assert not config.get("instructions")
    assert config["model"] == profile.settings.model
    assert config["providers"]["azure-ai"]["settings"]["apiKey"] == "{env:AZURE_API_KEY}"
    for tool in ("read", "skill", "bash", "edit", "notes_remember", "dwt_triage",
                 "dwt_close", "dwt_snooze", "dwt_relabel", "dwt_mute", "dwt_reopen"):
        assert permission(config, tool) == "deny"
    for tool in READ_TOOLS:
        assert permission(config, tool) == ("allow" if dwt_read_tools else "deny")


def test_ops_workspaces_are_fresh_and_prune_only_old_directories(profile):
    from assistant.render import ops_workspace

    old = ops_workspace(profile, dwt_read_tools=False)
    recent = ops_workspace(profile, dwt_read_tools=False)
    stale_time = time.time() - 86_401
    os.utime(old, (stale_time, stale_time))

    newest = ops_workspace(profile, dwt_read_tools=True)

    assert newest != recent != old
    assert not old.exists()
    assert recent.is_dir()
    assert newest.is_dir()


@pytest.mark.integration
@pytest.mark.skipif(not (os.environ.get("AZURE_API_KEY") and os.environ.get("ASSISTANT_IT_PROVIDER")),
                    reason="set AZURE_API_KEY and ASSISTANT_IT_PROVIDER to opt in")
def test_rendered_workspace_loads_harness_in_real_opencode(profile, monkeypatch):
    from assistant.opencode_env import run_opencode
    from assistant.render import render_workspace

    if not REAL_OPENCODE:
        pytest.skip("real opencode binary is unavailable")
    provider = json.loads(Path(os.environ["ASSISTANT_IT_PROVIDER"]).read_text())
    provider = provider.get("providers", {}).get("azure-ai", provider)
    monkeypatch.setenv("PATH", REAL_PATH)
    # Disable mail startup for this harness-loading check; never connect to real mail state.
    p = replace(profile, settings=replace(profile.settings, provider=provider,
                                          opencode_bin=REAL_OPENCODE, dwt_bin="/usr/bin/false"))
    p.settings.secrets_env.write_text("AZURE_API_KEY=" + os.environ["AZURE_API_KEY"] + "\n")
    p.settings.secrets_env.chmod(0o600)
    workspace = render_workspace(p, HARNESS_ROOT)

    result = run_opencode(p, "ops", ["run", "--auto", "--format", "json",
        "Elenca i nomi delle skill disponibili e riporta testualmente la prima regola "
        "della sezione Operating Guardrails già presente nelle istruzioni. Non usare strumenti."],
        cwd=workspace, timeout=180)

    assert result.returncode == 0, "real OpenCode run failed (output may contain private data)"
    events = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
    reply = "\n".join(event["part"]["text"] for event in events if event.get("type") == "text")
    assert "triage" in reply
    assert REQUIRED_GUARDRAIL_LINES[0].split(":")[0] in reply
