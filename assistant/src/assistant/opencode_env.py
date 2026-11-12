"""Build isolated OpenCode environments and run bounded child processes."""

from collections.abc import Mapping, Sequence
import os
from pathlib import Path
import re
import subprocess
import sys
from typing import Literal

from assistant.profile import Profile


Purpose = Literal["ui", "ops"]
_PARENT_KEYS = ("PATH", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR")


def warn_opencode_version(binary: str) -> None:
    """Version drift is visible to the operator but does not prevent startup."""
    try:
        result = subprocess.run(
            [binary, "--version"], timeout=10, stdin=subprocess.DEVNULL,
            capture_output=True, text=True,
        )
        version = re.search(r"\d+\.\d+\.\d+(?:-[\w.-]+)?(?:\+[\w.-]+)?", result.stdout)
        supported = result.returncode == 0 and version is not None and version.group() == "2.2.7"
    except (OSError, subprocess.SubprocessError):
        supported = False
    if not supported:
        # Do not echo arbitrary child output (which may contain private values).
        print("Assistente: avviso: il runtime richiede il comportamento di OpenCode 2.2.7; "
              "la versione installata è diversa o non verificabile.", file=sys.stderr)


def _azure_key(path: Path) -> str:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        raise FileNotFoundError(f"OpenCode secrets file not found: {path}") from None
    key = ""
    for line in lines:
        name, sep, value = line.strip().partition("=")
        if sep and name.strip() == "AZURE_API_KEY":
            value = value.strip()
            if len(value) >= 2 and value[0] in ("'", '"') and value[-1] == value[0]:
                value = value[1:-1]
            key = value
    if not key.strip():
        raise ValueError(f"OpenCode secrets file {path} must contain a non-empty AZURE_API_KEY")
    return key


def opencode_env(
    p: Profile,
    purpose: Purpose,
    *,
    workspace: Path | None = None,
    base: Mapping[str, str] | None = None,
) -> dict[str, str]:
    """Keep only A1's parent variables; create A2's isolated directories."""
    if purpose not in ("ui", "ops"):
        raise ValueError(f"unknown OpenCode purpose {purpose!r}; expected 'ui' or 'ops'")
    azure_key = _azure_key(p.settings.secrets_env)
    parent = os.environ if base is None else base
    env = {key: parent[key] for key in _PARENT_KEYS if key in parent}
    home = p.opencode_home
    store = home if purpose == "ui" else p.root / "opencode-ops"
    directories = {
        "HOME": home / "home",
        "XDG_CONFIG_HOME": home / "config",
        "XDG_DATA_HOME": store / "data",
        "XDG_CACHE_HOME": home / "cache",
        "XDG_STATE_HOME": store / "state",
        "TMPDIR": home / "tmp",
        "OPENCODE_CONFIG_DIR": home / "config-dir",
    }
    for key, directory in directories.items():
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        env[key] = str(directory)
    workspace = (p.workspace if workspace is None else workspace).resolve()
    workspace.mkdir(mode=0o700, parents=True, exist_ok=True)
    env.update(
        OPENCODE_CONFIG=str(workspace / "opencode.json"),
        OPENCODE_DISABLE_CLAUDE_CODE="true",
        NO_COLOR="1",
        AZURE_API_KEY=azure_key,
    )
    return env


def _connects(args: list[str]) -> bool:
    # Help/version exit locally even when qualified by a connecting command.
    options = args[:args.index("--")] if "--" in args else args
    if any(flag in options for flag in ("--help", "-h", "--version")):
        return False
    if not args:
        return False
    return args[0] in ("run", "session", "export", "api") or (
        len(args) > 1 and args[0] == "debug" and args[1] in ("config", "agents")
    )


def run_opencode(
    p: Profile,
    purpose: Purpose,
    args: list[str],
    *,
    cwd: Path,
    timeout: int,
    extra_env: Mapping[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    """Run with no stdin and captured text; explicit overrides support shims.

    ``extra_env`` is for trusted caller-supplied overrides, never a parent env.
    The secrets-file key and workspace discovery policy cannot be overridden.
    Nonzero exits are returned; timeouts raise subprocess.TimeoutExpired.
    """
    if type(timeout) is not int or timeout <= 0:
        raise ValueError("timeout must be a positive integer number of seconds")
    if extra_env:
        for key in ("AZURE_API_KEY", "OPENCODE_DISABLE_PROJECT_CONFIG"):
            if key in extra_env:
                raise ValueError(f"extra_env cannot override {key}")
    env = opencode_env(p, purpose, workspace=cwd)
    if extra_env:
        env.update(extra_env)
    command = [p.settings.opencode_bin, *args]
    options = args[:args.index("--")] if "--" in args else args
    if _connects(args) and "--standalone" not in options:
        command.insert(len(options) + 1, "--standalone")
    return subprocess.run(
        command, cwd=cwd, env=env, timeout=timeout,
        stdin=subprocess.DEVNULL, capture_output=True, text=True,
    )


def run_prompt(
    p: Profile,
    purpose: Purpose,
    prompt: str,
    *,
    cwd: Path,
    timeout: int,
    model: str | None = None,
    extra_args: Sequence[str] = (),
    extra_env: Mapping[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    """Pass a bounded prompt as data, including when it starts with a dash."""
    if len(prompt) > 200_000:
        raise ValueError("prompt must not exceed 200,000 characters")
    return run_opencode(
        p, purpose, ["run", "--format", "json", "--model", model or p.settings.model,
                     *extra_args, "--", prompt],
        cwd=cwd, timeout=timeout, extra_env=extra_env,
    )
