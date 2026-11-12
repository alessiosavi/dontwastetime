"""Render validated Markdown harnesses and isolated OpenCode configurations."""

from copy import deepcopy
from datetime import datetime
import json
from pathlib import Path
import shutil
import sys
import tempfile
import time
from types import SimpleNamespace

from assistant.h2 import harness2
from assistant.harness_rules import load_tree, validate_tree
from assistant.profile import Profile


_INSTRUCTIONS = (("notes.md", 4000), ("recap/latest.md", 3000), ("status.md", 1000))
_DWT_READ_TOOLS = (
    "dwt_context", "dwt_overview", "dwt_needs_reply", "dwt_list_messages",
    "dwt_search", "dwt_get_thread", "dwt_get_message", "dwt_digest", "dwt_health",
)


def _permissions(*actions: str) -> list[dict[str, str]]:
    return [{"action": "*", "resource": "*", "effect": "deny"}, *(
        {"action": action, "resource": "*", "effect": "allow"} for action in actions
    )]


def _base_config(p: Profile) -> dict:
    provider = deepcopy(p.settings.provider)
    provider.setdefault("settings", {})["apiKey"] = "{env:AZURE_API_KEY}"
    if p.settings.model_cost is not None:
        model = p.settings.model.split("/", 1)[-1].split("#", 1)[0]
        entry = provider.setdefault("models", {}).setdefault(model, {})
        rates = p.settings.model_cost
        entry["cost"] = {
            "input": rates["input"], "output": rates["output"],
            "cache": {"read": rates["cache_read"]},
        }
    return {
        "$schema": "https://opencode.ai/config.json",
        "model": p.settings.model,
        "providers": {"azure-ai": provider},
    }


def _local_server(command: list[str]) -> dict:
    return {"type": "local", "command": command, "codemode": False, "protocol": "legacy"}


def dwt_command(p: Profile, state_dir: Path | None = None, now: datetime | None = None) -> list[str]:
    """One argv for rendered MCP servers and the synchronous Python client."""
    if now is not None and now.utcoffset() is None:
        raise ValueError("now must be timezone-aware for dwt's RFC3339 clock")
    command = [
        p.settings.dwt_bin, "mcp", "--operator", "operator",
        "--config", str(p.settings.dwt_config), "--env", "/dev/null",
        "--state-dir", str(p.dwt_state_dir if state_dir is None else state_dir),
    ]
    if now is not None:
        command += ["--now", now.isoformat()]
    return command


def opencode_config(
    p: Profile,
    workspace: Path,
    *,
    dwt_state_dir: Path | None = None,
    now: datetime | None = None,
    notes_dir: Path | None = None,
) -> dict:
    """Build A4's live policy; no credentials or developer config are read."""
    memory = (p.memory_dir if notes_dir is None else notes_dir).resolve()
    return {
        **_base_config(p),
        "mcp": {"servers": {
            "dwt": _local_server(dwt_command(p, dwt_state_dir, now)),
            "notes": _local_server([
                sys.executable, "-m", "assistant", "notes-mcp", "--memory-dir", str(memory),
            ]),
        }},
        "permissions": _permissions("read", "skill", "dwt_*", "notes_*"),
        "instructions": [str(memory / path) for path, _ in _INSTRUCTIONS],
    }


def _check_rendered(rendered: Path) -> None:
    forbidden = [
        rendered / ".opencode/plugin",
        *(rendered / ".opencode/agent").glob("*.md"),
        *rendered.rglob("scripts"),
    ]
    for path in forbidden:
        if path.exists() or path.is_symlink():
            raise ValueError(f"forbidden rendered artifact: {path.relative_to(rendered)}")


def _check_budget(rendered: Path, config: dict) -> None:
    agents = len((rendered / "AGENTS.md").read_text(encoding="utf-8"))
    if agents > 16_000:
        raise ValueError(f"AGENTS.md is {agents} chars, over the 16000 cap")
    total = agents
    for path, (_, cap) in zip(config["instructions"], _INSTRUCTIONS, strict=True):
        file = Path(path)
        try:
            size = len(file.read_text(encoding="utf-8"))
        except FileNotFoundError:
            size = 0
        if size > cap:
            raise ValueError(f"{file} is {size} chars, over the {cap} cap")
        total += size
    if total > 24_000:
        raise ValueError(f"always-on content is {total} chars, over the 24000 cap")


def _check_destination(dest: Path, memory: Path) -> None:
    if dest.is_symlink() or (dest.exists() and not dest.is_dir()):
        raise ValueError("workspace destination must be a directory, not a symlink or file")
    resolved = dest.resolve()
    if memory.is_relative_to(resolved) or resolved.is_relative_to(memory):
        raise ValueError("workspace destination must not overlap the memory directory")
    link = dest / "memory"
    if link.is_symlink():
        if link.resolve() != memory:
            raise ValueError("workspace memory symlink must point to the profile memory directory")
    elif link.exists():
        raise ValueError("workspace memory must be a symlink to the profile memory directory")


def _publish_workspace(p: Profile, dest: Path, rendered: Path, config: dict) -> None:
    """Prepare copies beside dest, then replace it with rollback on rename failure."""
    config_text = json.dumps(config, ensure_ascii=False, indent=2) + "\n"
    dest.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = Path(tempfile.mkdtemp(prefix=".assistant-render-", dir=dest.parent))
    ready = temporary / "workspace"
    preserve_backup = False
    try:
        ready.mkdir(mode=0o700)
        shutil.copy2(rendered / "AGENTS.md", ready / "AGENTS.md")
        shutil.copytree(rendered / ".opencode/skills", ready / ".opencode/skills")
        (ready / "opencode.json").write_text(config_text, encoding="utf-8")
        (ready / "memory").symlink_to(p.memory_dir.resolve(), target_is_directory=True)
        p.memory_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        for path in config["instructions"]:
            file = Path(path)
            file.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            # Exclusive creation leaves existing notes/recap/status untouched.
            try:
                with file.open("x", encoding="utf-8"):
                    pass
            except FileExistsError:
                pass
        backup = temporary / "previous"
        if dest.exists():
            dest.rename(backup)
        try:
            ready.rename(dest)
        except BaseException:
            if backup.exists():
                try:
                    backup.rename(dest)
                except BaseException as error:
                    preserve_backup = True
                    raise RuntimeError(
                        f"workspace rollback failed; previous workspace preserved at {backup}"
                    ) from error
            raise
    finally:
        if preserve_backup:
            # If even rollback is refused, keep the only copy and report where it is.
            shutil.rmtree(ready, ignore_errors=True)
        else:
            shutil.rmtree(temporary)


def render_workspace(
    p: Profile, tree_dir: Path, dest: Path | None = None, **cfg_overrides,
) -> Path:
    """Validate before touching dest; render only into disposable directories.

    Memory lives outside the workspace and is only linked into the replacement.
    Staging, rendering and copying failures preserve the existing workspace.
    """
    tree_dir = tree_dir.resolve()
    validate_tree(load_tree(tree_dir))
    dest = (p.workspace if dest is None else dest).absolute()
    _check_destination(dest, p.memory_dir.resolve())
    config = opencode_config(p, dest, **cfg_overrides)
    for path in config["instructions"]:
        if Path(path).resolve().is_relative_to(dest.resolve()):
            raise ValueError("instruction files must not overlap the replaced workspace")
    h2 = harness2()
    ctx: dict[str, str] = {}
    install = h2.OpencodeSubstrate().materialise(
        tree_dir, SimpleNamespace(install_mode="workspace_agents_md"), ctx,
    )
    try:
        # Staging dereferences links; check the exact snapshot the renderer will consume.
        validate_tree(load_tree(install.staging_dir))
        with tempfile.TemporaryDirectory(prefix="assistant-render-") as temporary:
            rendered = Path(temporary) / "rendered"
            h2.render_opencode_tree(install.staging_dir, rendered, ctx)
            _check_rendered(rendered)
            _check_budget(rendered, config)
            _publish_workspace(p, dest, rendered, config)
    finally:
        shutil.rmtree(install.staging_dir)
    return dest


def ops_workspace(p: Profile, *, dwt_read_tools: bool) -> Path:
    """Create a fresh ops config and prune directories older than one day."""
    config = {
        **_base_config(p),
        "permissions": _permissions(*(_DWT_READ_TOOLS if dwt_read_tools else ())),
        "mcp": {"servers": {"dwt": _local_server(dwt_command(p))} if dwt_read_tools else {}},
    }
    config_text = json.dumps(config, ensure_ascii=False, indent=2) + "\n"
    root = p.root / "ops-workspaces"
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    cutoff = time.time() - 86_400
    for old in root.iterdir():
        if not old.is_symlink() and old.is_dir() and old.stat().st_mtime < cutoff:
            shutil.rmtree(old)
    workspace = Path(tempfile.mkdtemp(prefix="ops-", dir=root))
    try:
        (workspace / "opencode.json").write_text(config_text, encoding="utf-8")
    except BaseException:
        shutil.rmtree(workspace)
        raise
    return workspace
