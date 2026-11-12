"""Tone drafts use only final model text and the dev profile's read-only tools."""

from dataclasses import replace
from datetime import datetime, timedelta
import json
from pathlib import Path
import subprocess
import sys

import pytest


CARD = (
    "Registro\nDiretto e cortese.\n\nSaluti\nBuongiorno.\n\nChiusure\nGrazie.\n\n"
    "Tu/Lei\nSecondo il rapporto.\n\nLingua\nItaliano.\n\n"
    "Tipi di risposta\nConferme brevi.\n\nFrasi tipiche\n«Grazie mille»."
)


def text_event(text):
    return {"type": "text", "part": {"type": "text", "text": text}}


def step_event(id, tokens_in, tokens_out, cache_read=0, cost=0):
    return {"type": "step_finish", "part": {
        "id": id, "type": "step-finish", "cost": cost,
        "tokens": {"input": tokens_in, "output": tokens_out, "cache": {"read": cache_read}},
    }}


def fake_run(fake_opencode, events, *, code=0):
    output = "\n".join(json.dumps(event, ensure_ascii=False) for event in events)
    fake_opencode.path.write_text(
        f"#!{sys.executable}\n"
        "import json, os, pathlib, sys\n"
        "data = {'argv': sys.argv[1:], 'env': dict(os.environ), 'cwd': os.getcwd(), "
        "'stdin': sys.stdin.read()}\n"
        f"pathlib.Path({str(fake_opencode.record)!r}).write_text(json.dumps(data))\n"
        f"print({output!r})\n"
        f"sys.exit({code})\n"
    )


def usage_rows(profile):
    return [json.loads(line) for line in (profile.root / "usage.jsonl").read_text().splitlines()]


def test_tone_card_written_from_final_message(profile, fake_opencode):
    from assistant.tone import TONE_PROMPT, generate_tone_card

    fake_run(fake_opencode, [
        text_event("Sto leggendo la posta: PRIVATE_BEFORE_FIRST_TOOL"),
        {"type": "tool_use", "part": {"type": "tool", "name": "dwt_list_messages"}},
        step_event("first", 10, 2),
        text_event("PRIVATE_BETWEEN_TOOLS"),
        {"type": "tool_use", "part": {
            "type": "tool", "name": "dwt_get_message",
            "state": {"content": [{"type": "text", "text": "PRIVATE_TOOL_OUTPUT"}]},
        }},
        text_event(CARD[:50]), text_event(CARD[50:]), step_event("last", 20, 3),
    ])
    path = generate_tone_card(profile)
    assert path == profile.root / "tone-card.draft.md"
    assert path.read_text() == CARD
    assert path.stat().st_mode & 0o777 == 0o600
    invocation = fake_opencode.invocation()
    argv = invocation["argv"]
    separator = argv.index("--")
    assert argv[:5] == ["run", "--format", "json", "--model", profile.settings.model]
    assert "--standalone" in argv[:separator]
    assert argv[separator + 1:] == [TONE_PROMPT]
    assert invocation["stdin"] == ""
    assert Path(invocation["env"]["XDG_DATA_HOME"]) == profile.root / "opencode-ops/data"
    workspace = Path(invocation["cwd"])
    assert workspace.parent == profile.root / "ops-workspaces"
    config = json.loads((workspace / "opencode.json").read_text())
    assert config["permissions"][0] == {"action": "*", "resource": "*", "effect": "deny"}
    allowed = {rule["action"] for rule in config["permissions"] if rule["effect"] == "allow"}
    assert {"dwt_list_messages", "dwt_get_message"} <= allowed
    assert all(action.startswith("dwt_") and "*" not in action for action in allowed)
    assert not allowed & {"dwt_mark_handled", "dwt_relabel", "dwt_snooze", "dwt_reopen"}
    command = config["mcp"]["servers"]["dwt"]["command"]
    assert command[command.index("--state-dir") + 1] == str(profile.dwt_state_dir)
    assert command[command.index("--config") + 1] == str(profile.settings.dwt_config)
    assert set(config["mcp"]["servers"]) == {"dwt"}


def test_tone_card_truncated_to_3000(profile, fake_opencode):
    from assistant.tone import generate_tone_card

    fake_run(fake_opencode, [text_event("è" * 3500)])
    path = generate_tone_card(profile)
    assert path.read_text() == "è" * 3000


def test_tone_card_refuses_operator_profile(profile, fake_opencode):
    from assistant.tone import generate_tone_card

    with pytest.raises(ValueError, match="dev.*copia del database"):
        generate_tone_card(replace(profile, name="operator"))
    assert not fake_opencode.record.exists()
    assert not (profile.root / "ops-workspaces").exists()
    assert not (profile.root / "tone-card.draft.md").exists()
    assert not (profile.root / "usage.jsonl").exists()


@pytest.mark.parametrize("cost", [0, 0.75])
def test_tone_card_usage_recorded(profile, fake_opencode, cost):
    from assistant.tone import generate_tone_card

    profile = replace(profile, settings=replace(
        profile.settings, model_cost={"input": 1, "output": 2, "cache_read": 0.5},
    ))
    first = step_event("first", 100, 10, 20, cost)
    fake_run(fake_opencode, [first, first, text_event(CARD), step_event("last", 200, 30, 40, cost)])
    generate_tone_card(profile)
    row, = usage_rows(profile)
    assert row["call_id"] == f"tone-card:{row['at']}"
    assert datetime.fromisoformat(row["at"]).utcoffset() == timedelta()
    assert row["model"] == profile.settings.model
    assert (row["tokens_in"], row["tokens_out"], row["tokens_cache_read"]) == (300, 40, 60)
    assert row["cost_usd"] == pytest.approx(2 * cost if cost else 0.00041)


@pytest.mark.parametrize("mode", ["nonzero", "empty", "last_tool", "error_event", "malformed"])
@pytest.mark.parametrize("existing", [False, True])
def test_tone_card_failure_preserves_draft_and_records_usage(profile, fake_opencode, mode, existing):
    from assistant.tone import generate_tone_card

    events = [step_event("paid", 100, 10, 20), text_event(CARD)]
    if mode == "empty":
        events[-1] = text_event(" \n\t ")
    elif mode == "last_tool":
        events.append({"type": "tool_use", "part": {"type": "tool"}})
    elif mode == "error_event":
        events.append({"type": "error", "error": {"message": "PRIVATE_ERROR"}})
    elif mode == "malformed":
        events.append([])
    fake_run(fake_opencode, events, code=7 if mode == "nonzero" else 0)
    path = profile.root / "tone-card.draft.md"
    if existing:
        path.write_text("Previous reviewed draft")
    with pytest.raises(RuntimeError):
        generate_tone_card(profile)
    if existing:
        assert path.read_text() == "Previous reviewed draft"
    else:
        assert not path.exists()
    row, = usage_rows(profile)
    assert (row["tokens_in"], row["tokens_out"], row["tokens_cache_read"]) == (100, 10, 20)
    assert row["cost_usd"] is None


@pytest.mark.parametrize("mode", ["timeout", "oserror"])
def test_tone_card_runner_failure_records_partial_usage(profile, monkeypatch, mode):
    from assistant import opencode_env
    from assistant.tone import generate_tone_card

    def fail(p, purpose, prompt, *, cwd, timeout):
        assert purpose == "ops" and timeout == 900
        if mode == "timeout":
            output = json.dumps(step_event("paid", 100, 10, 20)) + "\n{unfinished"
            raise subprocess.TimeoutExpired("opencode", timeout, output=output.encode())
        raise OSError("PRIVATE_FAILURE")

    monkeypatch.setattr(opencode_env, "run_prompt", fail)
    with pytest.raises(RuntimeError):
        generate_tone_card(profile)
    assert not (profile.root / "tone-card.draft.md").exists()
    row, = usage_rows(profile)
    assert row["tokens_in"] == (100 if mode == "timeout" else 0)


@pytest.mark.parametrize("global_options", [False, True])
def test_tone_card_cli(profile, fake_opencode, capsys, global_options):
    from assistant.cli import main

    fake_run(fake_opencode, [text_event(CARD)])
    options = ["--profile", "dev", "--state-root", str(profile.root.parents[1])]
    argv = [*options, "tone-card"] if global_options else ["tone-card", *options]
    assert main(argv) == 0
    assert str(profile.root / "tone-card.draft.md") in capsys.readouterr().out
    assert (profile.root / "tone-card.draft.md").read_text() == CARD


@pytest.mark.parametrize("profile_options", [[], ["--profile", "operator"]])
def test_tone_card_cli_rejects_live_profile_before_loading(profile, fake_opencode, capsys, profile_options):
    from assistant.cli import main

    assert main(["tone-card", *profile_options, "--state-root", str(profile.root.parents[1])]) == 1
    error = capsys.readouterr().err
    assert len(error.splitlines()) == 1 and "dev" in error and "copia del database" in error
    assert not (profile.root.parent / "operator").exists()
    assert not fake_opencode.record.exists()
