"""Redirect Harness²'s bun invocation to the isolated installed OpenCode."""

from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import selectors
import shlex
import signal
import subprocess
import sys
import time

from assistant.notes_mcp import _atomic_write
from assistant.opencode_env import _PARENT_KEYS, _azure_key
from assistant.profile import Profile, Settings
from assistant.render import _base_config, _permissions
from assistant.spend import record_usage


def prepare_harness2_root(p: Profile) -> Path:
    """Supply only the files the unmodified installed proposer looks for."""
    root = (p.bench_dir / "h2").resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if (root / ".env").exists() or (root / ".env").is_symlink():
        raise ValueError("La directory Harness² non deve contenere un file .env.")
    improver = root / "improver"
    directories = {key: str(improver / name) for key, name in (
        ("HOME", "home"), ("XDG_CONFIG_HOME", "config"), ("XDG_CACHE_HOME", "cache"),
        ("TMPDIR", "tmp"), ("OPENCODE_CONFIG_DIR", "config-dir"),
    )}
    for path in directories.values():
        Path(path).mkdir(mode=0o700, parents=True, exist_ok=True)
    config_path = improver / "opencode.json"
    config = {
        **_base_config(p),
        "permissions": [*_permissions("read", "glob", "grep", "list"), *(
            {"action": "edit", "resource": resource, "effect": "allow"}
            for resource in ("context/harness/*", "output/answer.md")
        )],
    }
    _atomic_write(config_path, json.dumps(config, ensure_ascii=False, indent=2) + "\n")
    shim_config = root / "shim.json"
    _atomic_write(shim_config, json.dumps({
        "opencode_bin": p.settings.opencode_bin, "config": str(config_path),
        "directories": directories, "secrets_env": str(p.settings.secrets_env),
        "root": str(p.root), "profile": p.name, "model": p.settings.model,
        "model_cost": p.settings.model_cost, "timeout_seconds": 1750,
    }, ensure_ascii=False, indent=2) + "\n")
    bun = root / "dependencies/tools/node_modules/.bin/bun"
    bun.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    # A shell exec handles Python paths containing spaces (e.g. Application Support).
    _atomic_write(bun, "#!/bin/sh\nexec " + shlex.join([
        sys.executable, "-m", "assistant.bench.shim", "--shim-config", str(shim_config),
    ]) + ' "$@"\n')
    bun.chmod(0o700)
    placeholder = root / "dependencies/opencode/packages/opencode/src/index.ts"
    placeholder.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    _atomic_write(placeholder, "// Placeholder: the bun shim runs the installed OpenCode.\n")
    return root


def _translate(argv: list[str], config: dict) -> tuple[list[str], Path, str]:
    if (len(argv) < 7 or argv[:2] != ["run", "--cwd"] or not argv[2]
            or argv[3:6] != ["--conditions=browser", "src/index.ts", "run"]):
        raise ValueError("argv bun inattesi: serve run --cwd X --conditions=browser src/index.ts run PROMPT")
    prompt = argv[6]
    if len(prompt) > 200_000:
        raise ValueError("argv: il prompt supera 200.000 caratteri")
    values = {"--model": config["model"], "--dir": str(Path.cwd()), "--format": "json"}
    index = 7
    while index < len(argv):
        flag, separator, value = argv[index].partition("=")
        if not flag.startswith("--") or flag == "--":
            raise ValueError("argv: argomento inatteso dopo il prompt")
        index += 1
        if not separator and index < len(argv) and not argv[index].startswith("--"):
            value = argv[index]
            index += 1
        if flag in (*values, "--variant") and not value:
            raise ValueError(f"argv: manca il valore di {flag}")
        if flag in values:
            values[flag] = value
        # Variant and other upstream flags are deliberately not forwarded.
    if values["--format"] != "json":
        raise ValueError("argv: Harness² richiede --format json")
    model = values["--model"]
    return ([config["opencode_bin"], "run", "--standalone", "--format", "json", "--model", model,
             "--", prompt], Path(values["--dir"]).resolve(), model)


def _environment(config: dict) -> dict[str, str]:
    env = {key: os.environ[key] for key in _PARENT_KEYS if key in os.environ}
    env.update(config["directories"])
    for key in ("XDG_DATA_HOME", "XDG_STATE_HOME"):
        if not os.environ.get(key):
            raise ValueError(f"argv: manca {key} della sessione Harness²")
        env[key] = os.environ[key]
    env.update(OPENCODE_CONFIG=config["config"], OPENCODE_DISABLE_CLAUDE_CODE="true",
               NO_COLOR="1", AZURE_API_KEY=_azure_key(Path(config["secrets_env"])))
    return env


class _Usage:
    """Fold complete JSONL steps while leaving every relayed byte untouched."""

    def __init__(self):
        self.pending = b""
        self.session = ""
        self.seen = set()
        self.steps = 0
        self.tokens_in = self.tokens_out = self.tokens_cache_read = 0
        self.cost = 0.0

    def feed(self, chunk: bytes, *, final: bool = False) -> None:
        lines = (self.pending + chunk).split(b"\n")
        self.pending = b"" if final else lines.pop()
        for line in lines:
            try:
                event = json.loads(line)
                part = event.get("part", {})
                session = event.get("sessionID") or part.get("sessionID")
                if isinstance(session, str) and session:
                    self.session = session
                if (part.get("type") or event.get("type")) not in ("step-finish", "step_finish"):
                    continue
                identifier = part.get("id")
                if identifier and identifier in self.seen:
                    continue
                tokens = part.get("tokens", {})
                counts = [tokens.get("input", 0), tokens.get("output", 0), tokens.get("cache", {}).get("read", 0)]
                cost = part.get("cost") or 0.0
                if (any(type(n) is not int or n < 0 for n in counts)
                        or type(cost) not in (int, float) or not math.isfinite(cost) or cost < 0):
                    continue
                if identifier:
                    self.seen.add(identifier)
                self.steps += 1
                self.tokens_in += counts[0]
                self.tokens_out += counts[1]
                self.tokens_cache_read += counts[2]
                self.cost += cost
            except (ValueError, TypeError, AttributeError):
                continue


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    try:
        if len(argv) < 2 or argv[0] != "--shim-config":
            raise ValueError("argv: manca --shim-config PATH del launcher bun")
        config = json.loads(Path(argv[1]).read_text(encoding="utf-8"))
        command, cwd, model = _translate(argv[2:], config)
        env = _environment(config)
        timeout = config.get("timeout_seconds", 1750)
        if type(timeout) not in (int, float) or not math.isfinite(timeout) or not 0 < timeout < 1800:
            raise ValueError("timeout_seconds deve essere positivo e inferiore a 1800.")
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(f"Shim Harness²: {error}", file=sys.stderr)
        return 2
    at = datetime.now(timezone.utc)
    usage = _Usage()
    process, interrupted, broken_pipe = None, 0, False
    handlers = {}

    def forward(number, frame):
        nonlocal interrupted
        interrupted = number
        if process is not None:
            try:
                os.killpg(process.pid, number)
            except ProcessLookupError:
                pass

    def relay(chunk):
        nonlocal broken_pipe
        usage.feed(chunk)
        try:
            sys.stdout.buffer.write(chunk)
            sys.stdout.buffer.flush()
        except BrokenPipeError:
            broken_pipe = True

    try:
        for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            handlers[number] = signal.signal(number, forward)
        process = subprocess.Popen(command, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, start_new_session=True, bufsize=0)
        # stderr inherits the shim's descriptor. Poll the pipe so a silent child
        # cannot prevent enforcement of our deadline or signal cleanup.
        deadline = time.monotonic() + timeout
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while process.poll() is None:
                if interrupted:
                    code = 128 + interrupted
                    # Give forwarded signals a bounded chance to run handlers.
                    try:
                        process.wait(timeout=0.2)
                    except subprocess.TimeoutExpired:
                        pass
                    break
                if broken_pipe:
                    code = 128 + signal.SIGPIPE
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    code = 124
                    break
                for key, _ in selector.select(min(remaining, 0.1)):
                    chunk = os.read(key.fd, 65_536)
                    if chunk:
                        relay(chunk)
                    else:
                        selector.unregister(key.fileobj)
            else:
                code = 128 + interrupted if interrupted else process.returncode
    finally:
        try:
            if process is not None:
                # Kill descendants even if the leader exited successfully or
                # closed stdout. Reap our direct child on every exit path.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait()
                os.set_blocking(process.stdout.fileno(), False)
                while True:
                    try:
                        chunk = os.read(process.stdout.fileno(), 65_536)
                    except BlockingIOError:
                        break
                    if not chunk:
                        break
                    relay(chunk)
                process.stdout.close()
        finally:
            usage.feed(b"", final=True)
            if usage.session and usage.steps:
                try:
                    p = Profile(config["profile"], Path(config["root"]),
                                Settings(model=config["model"], model_cost=config["model_cost"]))
                    record_usage(p, f"improver:{usage.session}", at, model, usage.tokens_in, usage.tokens_out,
                                 usage.cost, tokens_cache_read=usage.tokens_cache_read)
                except (OSError, ValueError, KeyError, TypeError) as error:
                    print(f"Shim Harness²: registrazione usage fallita ({type(error).__name__}).", file=sys.stderr)
            for number, handler in handlers.items():
                signal.signal(number, handler)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
