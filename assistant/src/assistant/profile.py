"""Profile settings and paths, independent of OpenCode's developer setup."""

from dataclasses import dataclass, field
import json
import os
from pathlib import Path
from typing import Literal, cast


ProfileName = Literal["operator", "dev"]


@dataclass(frozen=True)
class Settings:
    model: str = "azure-ai/DeepSeek-V4.1-Flash-1017"
    improve_every_hours: int = 24
    soft_cap_usd: float = 100.0
    opencode_bin: str = "opencode"
    dwt_bin: str = "dwt"
    dwt_config: Path = Path("config.json")
    dwt_env: Path = Path(".env")
    secrets_env: Path = Path("secrets.env")
    provider: dict = field(default_factory=dict)
    model_cost: dict | None = None


@dataclass(frozen=True)
class Profile:
    name: ProfileName
    root: Path
    settings: Settings

    @property
    def workspace(self) -> Path:
        return self.root / "workspace"

    @property
    def opencode_home(self) -> Path:
        return self.root / "opencode"

    @property
    def memory_dir(self) -> Path:
        return self.root / "memory"

    @property
    def exports_dir(self) -> Path:
        return self.root / "exports"

    @property
    def snapshots_dir(self) -> Path:
        return self.root / "snapshots"

    @property
    def bench_dir(self) -> Path:
        return self.root / "bench"

    @property
    def logs_dir(self) -> Path:
        return self.root / "logs"

    @property
    def markers_path(self) -> Path:
        return self.root / "markers.json"

    @property
    def dwt_state_dir(self) -> Path:
        if self.name == "dev":
            return self.root / "dwt"
        return _dwt_state_dir(self.settings.dwt_config)


def _read_object(path: Path) -> dict:
    try:
        text = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return {}
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        raise ValueError(f"{path}: expected a JSON object") from None
    if not isinstance(data, dict):
        raise ValueError(f"{path}: expected a JSON object")
    return data


def _dwt_state_dir(config: Path) -> Path:
    state_dir = _read_object(config).get("state_dir")
    if not state_dir:
        return Path.home() / "Library" / "Application Support" / "dontwastetime"
    if not isinstance(state_dir, str):
        raise ValueError(f"{config}: state_dir must be a path string")
    return Path(state_dir).expanduser().resolve()


def load_profile(name: str, state_root: Path | None = None) -> Profile:
    """Load <dwt state>/assistant/<name>/settings.json, or use defaults.

    ``state_root`` overrides the dwt state directory used to locate profiles.
    Paths in settings are relative to the profile root unless absolute or ~/… .
    No credential file is read here; only opencode_env loads secrets.env.
    """
    if name not in ("operator", "dev"):
        raise ValueError(f"unknown profile {name!r}; expected 'operator' or 'dev'")

    live_config = Path(os.environ.get("DWT_CONFIG") or "config.json").expanduser().resolve()
    state = _dwt_state_dir(live_config) if state_root is None else state_root
    root = Path(state).expanduser().resolve() / "assistant" / name
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    root.chmod(0o700)
    settings_path = root / "settings.json"
    values = _read_object(settings_path)

    default_config = root / "dwt" / "config.json" if name == "dev" else live_config
    for key, default in (
        ("dwt_config", default_config),
        ("dwt_env", None),
        ("secrets_env", root / "secrets.env"),
    ):
        if key == "dwt_env":
            default = values["dwt_config"].parent / ".env"
        value = values.get(key, default)
        if not isinstance(value, (str, Path)) or not str(value):
            raise ValueError(f"{settings_path}: {key} must be a path string")
        path = Path(value).expanduser()
        values[key] = (root / path).resolve()

    if not isinstance(values.get("provider", {}), dict):
        raise ValueError(f"{settings_path}: provider must be an object")
    if values.get("model_cost") is not None and not isinstance(values["model_cost"], dict):
        raise ValueError(f"{settings_path}: model_cost must be an object or null")
    try:
        settings = Settings(**values)
    except TypeError as error:
        raise ValueError(f"{settings_path}: unsupported settings field") from error
    return Profile(cast(ProfileName, name), root, settings)
