"""Install a private assistant profile without changing dwt's live files."""

from contextlib import closing
from copy import deepcopy
from dataclasses import asdict, replace
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import plistlib
import re
import shlex
import shutil
import sqlite3
import subprocess
import tempfile

from assistant.harness_rules import load_tree, validate_tree
from assistant.profile import Profile, _dwt_state_dir, _read_object, load_profile
from assistant.opencode_env import warn_opencode_version
from assistant.render import render_workspace
from assistant.snapshot import ui_active


def _atomic_write(path: Path, text: str, mode: int = 0o600) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}-", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            os.fchmod(output.fileno(), mode)
            output.write(text)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def installation_time(p: Profile, now: datetime) -> datetime:
    """Stable fallback for profiles installed before the installed_at marker."""
    path = p.root / "settings.json"
    stat = (path if path.exists() else p.root).stat()
    return min(now, datetime.fromtimestamp(getattr(stat, "st_birthtime", stat.st_mtime), timezone.utc))


def _jsonc(text: str) -> dict:
    """Strip comments and trailing commas only outside JSON string literals."""
    cleaned = []
    i = 0
    quoted = False
    escaped = False
    while i < len(text):
        char = text[i]
        if quoted:
            cleaned.append(char)
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quoted = False
        elif char == '"':
            quoted = True
            cleaned.append(char)
        elif text[i:i + 2] == "//":
            end = text.find("\n", i + 2)
            i = len(text) if end == -1 else end
            cleaned.append("\n")
            continue
        elif text[i:i + 2] == "/*":
            end = text.find("*/", i + 2)
            if end == -1:
                raise ValueError("Provider JSONC: commento non terminato.")
            cleaned.append(" ")
            i = end + 2
            continue
        else:
            cleaned.append(char)
        i += 1
    text = "".join(cleaned)
    cleaned = []
    quoted = escaped = False
    for i, char in enumerate(text):
        if quoted:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quoted = False
        elif char == '"':
            quoted = True
        elif char == "," and text[i + 1:].lstrip().startswith(("}", "]")):
            continue
        cleaned.append(char)
    try:
        result = json.loads("".join(cleaned))
    except json.JSONDecodeError:
        # The decoder's message/context must never reveal provider values.
        raise ValueError("Provider: JSON/JSONC non valido.") from None
    if not isinstance(result, dict):
        raise ValueError("Provider: è richiesto un oggetto JSON.")
    return result


def _check_no_literal_secrets(value) -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if (re.search(r"key|token|secret|password", key, re.IGNORECASE)
                    and isinstance(child, str)
                    and not re.fullmatch(r"\{env:[A-Za-z_][A-Za-z0-9_]*\}", child)):
                raise ValueError("Provider: credenziale letterale vietata; usa {env:NOME_VARIABILE}.")
            _check_no_literal_secrets(child)
    elif isinstance(value, list):
        for child in value:
            _check_no_literal_secrets(child)


def _provider(value: dict, model: str) -> dict:
    if "providers" in value:
        providers = value["providers"]
        value = providers.get("azure-ai") if isinstance(providers, dict) else None
    if not isinstance(value, dict):
        raise ValueError("Provider azure-ai non trovato; passa --provider-json <file>.")
    _check_no_literal_secrets(value)
    name = model.split("/", 1)[-1].split("#", 1)[0]
    models = value.get("models")
    if not isinstance(models, dict) or not isinstance(models.get(name), dict):
        raise ValueError("Il provider azure-ai non contiene il modello configurato.")
    result = deepcopy(value)
    result["models"] = {name: result["models"][name]}
    return result


def _label(p: Profile) -> str:
    return f"com.local.dontwastetime.assistant.{p.name}"


def _plist_path(p: Profile) -> Path:
    return Path.home() / "Library" / "LaunchAgents" / f"{_label(p)}.plist"


def launcher_script(p: Profile) -> str:
    """Return a cwd-independent bash launcher; no files are read or created."""
    command = [
        str(p.root / "venv/bin/python"), "-m", "assistant", "launch",
        "--profile", p.name, "--state-root", str(p.root.parent.parent),
    ]
    return "#!/bin/bash\nexec " + shlex.join(command) + "\n"


def launchd_plist(p: Profile) -> str:
    """Return the profile's due-based tick job without side effects."""
    return plistlib.dumps({
        "Label": _label(p),
        "ProgramArguments": [
            str(p.root / "venv/bin/python"), "-m", "assistant", "tick",
            "--profile", p.name, "--state-root", str(p.root.parent.parent),
        ],
        "WorkingDirectory": str(p.root),
        "RunAtLoad": True,
        "StartInterval": 900,
        "StartCalendarInterval": {"Hour": 9, "Minute": 0},
        "ProcessType": "Background",
        "StandardOutPath": str(p.logs_dir / "tick.log"),
        "StandardErrorPath": str(p.logs_dir / "tick.log"),
        "EnvironmentVariables": {"PATH": "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"},
    }).decode("utf-8")


def _bootout(p: Profile) -> None:
    subprocess.run(
        ["launchctl", "bootout", f"gui/{os.getuid()}/{_label(p)}"],
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        check=False,
    )


def _backup(source: Path, dest: Path) -> None:
    """Use SQLite's online backup API; never query or write the source DB."""
    # Keep the destination inode and let SQLite transact against any existing
    # WAL/SHM. Replacing only its main file would leave old WAL contents visible.
    with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True)) as live:
        with closing(sqlite3.connect(dest)) as copy:
            live.backup(copy)


def _valid_email(value: str | None) -> bool:
    return bool(value and re.fullmatch(
        r"[A-Za-z0-9][A-Za-z0-9._%+\-]*@[A-Za-z0-9]+(?:[.\-][A-Za-z0-9]+)*\.[A-Za-z]{2,63}", value,
    ) and ".." not in value and not value.split("@", 1)[0].endswith("."))


def install(
    profile: str, *, repo: Path, state_root: Path | None = None,
    dev_recipient: str | None = None, desktop_launcher: bool = False,
    provider_json: Path | None = None, no_launchd: bool = False,
    recap_recipient: str | None = None,
) -> Profile:
    """Refresh generated files, retaining user settings and persistent memory."""
    from assistant.markers import load_markers, save_markers

    now = datetime.now(timezone.utc)
    repo = Path(repo).expanduser().resolve()
    live_config = repo / "config.json"
    live_state = _dwt_state_dir(live_config, repo / ".env")
    p = load_profile(profile, state_root if state_root is not None else live_state)
    if ui_active(p):
        raise RuntimeError("Chiudi l'assistente prima di aggiornarlo.")
    if p.name == "dev" and not _valid_email(dev_recipient):
        raise ValueError("Il profilo dev richiede --dev-recipient con un indirizzo email valido.")
    key = os.environ.get("AZURE_API_KEY", "")
    if not key.strip() or "\n" in key or "\r" in key:
        raise ValueError("Imposta AZURE_API_KEY nell'ambiente prima dell'installazione (una sola riga).")
    dwt = repo / "bin/dwt"
    if not dwt.is_file():
        raise FileNotFoundError("bin/dwt non trovato: esegui make build nel repository.")
    opencode = shutil.which("opencode")
    if not opencode:
        raise FileNotFoundError("opencode non trovato nel PATH.")
    uv = shutil.which("uv")
    if not uv:
        raise FileNotFoundError("uv non trovato nel PATH.")
    if provider_json is not None:
        provider = _provider(_jsonc(Path(provider_json).expanduser().read_text(encoding="utf-8")), p.settings.model)
    elif p.settings.provider:
        provider = _provider(p.settings.provider, p.settings.model)
    else:
        config = Path.home() / ".config/opencode/opencode.jsonc"
        if not config.is_file():
            raise FileNotFoundError("Provider non trovato: passa --provider-json <file>.")
        provider = _provider(_jsonc(config.read_text(encoding="utf-8")), p.settings.model)

    if not live_config.is_file():
        raise FileNotFoundError("config.json non trovato nel repository.")
    config = _read_object(live_config)
    if p.name == "dev":
        recap_recipient = dev_recipient
    else:
        from assistant.recap import recap_recipient as resolve_recipient

        stored = p.settings.recap_recipient
        recipients = config.get("digest_recipients")
        if (recap_recipient is None and stored and isinstance(recipients, list)
                and any(isinstance(value, str) and value.casefold() == stored.casefold()
                        for value in recipients)):
            recap_recipient = stored
        candidate = replace(p, settings=replace(
            p.settings, dwt_config=live_config, recap_recipient=recap_recipient,
        ))
        try:
            recap_recipient = resolve_recipient(candidate)
        except ValueError:
            raise ValueError("Passa --recap-recipient con un indirizzo presente in digest_recipients.") from None
    live_db = live_state / "dwt.db"
    dev_dir = p.root / "dwt"
    if p.name == "dev":
        if not live_db.is_file():
            raise FileNotFoundError("Il database dwt.db da copiare non esiste.")
        dev_db = dev_dir / "dwt.db"
        if (dev_dir.is_symlink() or dev_db.is_symlink()
                or (dev_db.exists() and dev_db.samefile(live_db))):
            raise ValueError("La copia dev deve essere separata dal database live.")
        if not (repo / "taxonomy.json").is_file():
            raise FileNotFoundError("taxonomy.json non trovato nel repository.")

    settings = replace(
        p.settings, opencode_bin=str(Path(opencode).absolute()), dwt_bin=str(dwt),
        dwt_config=dev_dir / "config.json" if p.name == "dev" else live_config,
        dwt_env=repo / ".env", secrets_env=p.root / "secrets.env", provider=provider,
        recap_recipient=recap_recipient,
        dwt_state_dir=str(dev_dir if p.name == "dev" else live_state),
    )
    p = replace(p, settings=settings)
    warn_opencode_version(settings.opencode_bin)
    subprocess.run([uv, "venv", "--python", "3.13", str(p.root / "venv")], check=True)
    subprocess.run([
        uv, "pip", "install", "--python", str(p.root / "venv/bin/python"), str(repo / "assistant"),
    ], check=True)
    _atomic_write(p.root / "settings.json", json.dumps(asdict(settings), default=str, indent=2) + "\n")
    _atomic_write(settings.secrets_env, f"AZURE_API_KEY={key}\n")
    harness = p.root / "harness-live"
    from assistant.bench.improve import fingerprint, record_promoted_install

    tree = load_tree(repo / "assistant/harness")
    validate_tree(tree)
    previous_harness = fingerprint(harness) if harness.is_dir() else None
    if harness.exists():
        shutil.rmtree(harness)
    harness.mkdir(mode=0o700, parents=True)
    for name, text in tree.items():
        _atomic_write(harness / name, text)
    if p.name == "dev":
        dev_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        _backup(live_db, dev_dir / "dwt.db")
        config.update(state_dir=str(dev_dir), digest_recipients=[dev_recipient], alert_recipients=[dev_recipient])
        _atomic_write(settings.dwt_config, json.dumps(config, indent=2) + "\n")
        _atomic_write(dev_dir / "taxonomy.json", (repo / "taxonomy.json").read_text(encoding="utf-8"))
    render_workspace(p, harness)
    record_promoted_install(p, previous_harness, now)
    script = launcher_script(p)
    _atomic_write(p.root / "Assistente.command", script, 0o755)
    if desktop_launcher:
        _atomic_write(Path.home() / "Desktop/Assistente.command", script, 0o755)
    markers = load_markers(p)
    if markers.installed_at is None:
        markers.installed_at = now
        save_markers(p, markers)
    if not no_launchd:
        p.logs_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        plist = _plist_path(p)
        _atomic_write(plist, launchd_plist(p))
        _bootout(p)
        subprocess.run(["launchctl", "bootstrap", f"gui/{os.getuid()}", str(plist)], check=True)
    return p


def uninstall(profile: str, *, state_root: Path | None = None, purge: bool = False) -> None:
    """Remove only this assistant's job and owned launchers; state is opt-in."""
    p = load_profile(profile, state_root)
    _bootout(p)
    _plist_path(p).unlink(missing_ok=True)
    desktop = Path.home() / "Desktop/Assistente.command"
    try:
        if desktop.read_text(encoding="utf-8") == launcher_script(p):
            desktop.unlink()
    except FileNotFoundError:
        pass
    (p.root / "Assistente.command").unlink(missing_ok=True)
    if purge:
        shutil.rmtree(p.root)
