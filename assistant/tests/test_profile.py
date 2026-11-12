from dataclasses import FrozenInstanceError
import json
from pathlib import Path
import stat

import pytest

from assistant.profile import load_profile


def test_load_profile_reads_settings_and_defaults(tmp_path):
    state = tmp_path / "state"
    root = state / "assistant" / "dev"
    root.mkdir(parents=True)
    provider = {"name": "Azure", "settings": {"apiKey": "{env:AZURE_API_KEY}"}}
    (root / "settings.json").write_text(json.dumps({"provider": provider}))

    p = load_profile("dev", state_root=state)

    assert p.root == root
    assert p.settings.model == "azure-ai/DeepSeek-V4.1-Flash-1017"
    assert p.settings.improve_every_hours == 24
    assert p.settings.soft_cap_usd == 100.0
    assert p.settings.opencode_bin == "opencode"
    assert p.settings.dwt_bin == "dwt"
    assert p.settings.provider == provider
    assert p.settings.model_cost is None
    assert p.settings.dwt_config == root / "dwt" / "config.json"
    assert p.settings.dwt_env == root / "dwt" / ".env"
    assert p.settings.secrets_env == root / "secrets.env"
    assert p.dwt_state_dir == root / "dwt"
    for prop, relative in {
        "workspace": "workspace", "opencode_home": "opencode",
        "memory_dir": "memory", "exports_dir": "exports",
        "snapshots_dir": "snapshots", "bench_dir": "bench",
        "logs_dir": "logs", "markers_path": "markers.json",
    }.items():
        assert getattr(p, prop) == root / relative
    with pytest.raises(FrozenInstanceError):
        p.name = "operator"
    with pytest.raises(FrozenInstanceError):
        p.settings.model = "changed"


def test_settings_overrides_and_paths(tmp_path):
    p = load_profile("dev", state_root=tmp_path)
    cost = {"input": 0.1, "output": 0.2, "cache_read": 0.01}
    (p.root / "settings.json").write_text(json.dumps({
        "model": "test/model", "improve_every_hours": 48, "soft_cap_usd": 12.5,
        "opencode_bin": "/fake/opencode", "dwt_bin": "/fake/dwt",
        "dwt_config": "config-copy.json", "dwt_env": "~/graph.env",
        "secrets_env": str(tmp_path / "fake-secrets.env"), "model_cost": cost,
    }))
    p = load_profile("dev", state_root=tmp_path)
    assert p.settings.model == "test/model"
    assert p.settings.improve_every_hours == 48
    assert p.settings.soft_cap_usd == 12.5
    assert p.settings.opencode_bin == "/fake/opencode"
    assert p.settings.dwt_bin == "/fake/dwt"
    assert p.settings.dwt_config == p.root / "config-copy.json"
    assert p.settings.dwt_env == Path.home() / "graph.env"
    assert p.settings.secrets_env == tmp_path / "fake-secrets.env"
    assert p.settings.model_cost == cost
    assert p.dwt_state_dir == p.root / "dwt"


def test_profile_root_is_0700(tmp_path):
    p = load_profile("dev", state_root=tmp_path)
    assert stat.S_IMODE(p.root.stat().st_mode) == 0o700
    p.root.chmod(0o755)
    load_profile("dev", state_root=tmp_path)
    assert stat.S_IMODE(p.root.stat().st_mode) == 0o700


@pytest.mark.parametrize("name", ["other", "", "../dev"])
def test_unknown_profile_rejected(tmp_path, name):
    with pytest.raises(ValueError, match="profile"):
        load_profile(name, state_root=tmp_path / "unused")
    assert not (tmp_path / "unused").exists()


def test_default_root_uses_dwt_config_state_dir(tmp_path, monkeypatch):
    state = tmp_path / "live-state"
    config = tmp_path / "live-config.json"
    config.write_text(json.dumps({"state_dir": str(state)}))
    monkeypatch.setenv("DWT_CONFIG", str(config))
    p = load_profile("operator")
    assert p.root == state / "assistant" / "operator"
    assert p.settings.dwt_config == config
    assert p.settings.dwt_env == tmp_path / ".env"
    assert p.dwt_state_dir == state
    dev = load_profile("dev")
    assert dev.root == state / "assistant" / "dev"
    assert dev.dwt_state_dir == dev.root / "dwt"


def test_default_state_dir_without_config():
    p = load_profile("operator")
    state = Path.home() / "Library" / "Application Support" / "dontwastetime"
    assert p.root == state / "assistant" / "operator"
    assert p.dwt_state_dir == state


def test_operator_state_dir_comes_from_profile_config(tmp_path):
    p = load_profile("operator", state_root=tmp_path)
    config = tmp_path / "custom.json"
    state = tmp_path / "custom-live-state"
    config.write_text(json.dumps({"state_dir": str(state)}))
    (p.root / "settings.json").write_text(json.dumps({"dwt_config": str(config)}))
    p = load_profile("operator", state_root=tmp_path)
    assert p.dwt_state_dir == state
    config.write_text("{}")
    assert p.dwt_state_dir == Path.home() / "Library/Application Support/dontwastetime"


@pytest.mark.parametrize("contents", ["{bad", "[]"])
def test_invalid_settings_fail_clearly(tmp_path, contents):
    p = load_profile("dev", state_root=tmp_path)
    (p.root / "settings.json").write_text(contents)
    with pytest.raises(ValueError, match="settings.json"):
        load_profile("dev", state_root=tmp_path)
