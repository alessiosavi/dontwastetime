"""Recap facts and delivery receipts are verified without mail or paid calls."""

from dataclasses import replace
from datetime import date, datetime, timedelta, timezone
import json
import os
from pathlib import Path
import stat
import subprocess
import sys

import pytest

from assistant.markers import Markers, load_markers, save_markers
from assistant.notes_mcp import NotesStore
from assistant.sessions import ToolCall
from assistant.status import pending_status, record_status


NOW = datetime(2026, 11, 12, 9, tzinfo=timezone.utc)
START = NOW - timedelta(days=1)
FIXTURE = Path(__file__).parent / "fixtures/run_events.jsonl"


class MailState:
    def __init__(self, *, count=7, synced=NOW):
        self.items = [
            {"id": n, "untrusted": {"from_name": f"Mittente {n}", "subject": f"Oggetto {n}"},
             "due_at": "2026-11-13T08:00:00Z"} for n in range(1, count + 1)
        ]
        self.synced = synced
        self.calls = []

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        pass

    def call(self, tool, args=None):
        self.calls.append((tool, args))
        if tool == "health":
            return {"mailboxes": [
                {"last_sync_completed_at": NOW.isoformat()},
                {"last_sync_completed_at": self.synced.isoformat() if self.synced else None},
            ]}
        assert tool == "needs_reply"
        offset = int((args or {}).get("cursor", 0))
        page = self.items[offset:offset + 100]
        cursor = str(offset + 100) if offset + 100 < len(self.items) else ""
        return {"items": page, "meta": {"next_cursor": cursor}}


def export_turn(profile, *, at=START, calls=(), message="Chiudi questa", reply="Ho chiuso.", sid="ses_ui"):
    profile.exports_dir.mkdir(exist_ok=True)
    content = [{"type": "text", "text": reply}]
    content += [
        {"type": "tool", "name": f"{call.server}_{call.tool}",
         "state": {"input": call.args, "status": "completed" if call.ok else "error",
                   "output": call.output}} for call in calls
    ]
    model = {"providerID": "azure-ai", "id": "DeepSeek-V4.1-Flash-1017"}
    (profile.exports_dir / f"{sid}.json").write_text(json.dumps({
        "info": {"id": sid, "model": model}, "messages": [
            {"id": "user", "type": "user", "text": message,
             "time": {"created": at.timestamp() * 1000}},
            {"id": "reply", "type": "assistant", "content": content, "model": model,
             "time": {"created": (at + timedelta(seconds=1)).timestamp() * 1000}},
        ],
    }))


@pytest.fixture
def recap_dwt(profile, fake_dwt):
    config = profile.settings.dwt_config
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text(json.dumps({"digest_recipients": ["dev@example.com"]}))
    control = profile.root / "send-control.json"
    control.write_text('{"exit": 0}')
    trace = profile.root / "send-trace.jsonl"
    fake_dwt.path.write_text(
        f"#!{sys.executable}\n"
        "import json, os, pathlib, signal, sys, time\n"
        f"root = pathlib.Path({str(profile.root)!r})\n"
        "args = sys.argv[1:]\n"
        "assert args[0] == 'send-note'\n"
        "path = pathlib.Path(args[args.index('--file')+1])\n"
        "row = {'argv': args, 'text': path.read_text(), 'stdin': sys.stdin.read(),\n"
        "       'latest': (root/'memory/recap/latest.md').read_text(),\n"
        "       'markers': json.loads((root/'markers.json').read_text())}\n"
        f"with pathlib.Path({str(trace)!r}).open('a') as out: out.write(json.dumps(row)+'\\n')\n"
        f"control = json.loads(pathlib.Path({str(control)!r}).read_text())\n"
        "if control.get('signal'): os.kill(os.getpid(), signal.SIGKILL)\n"
        "if control.get('sleep'): time.sleep(control['sleep'])\n"
        "print('private diagnostic must not enter recap.log', file=sys.stderr)\n"
        "sys.exit(control.get('exit', 0))\n"
    )
    return control, trace


def recap_value(text="Testo del recap", **kwargs):
    from assistant.recap import Recap

    return Recap("Recap assistente — giovedì 12 novembre", text, START, NOW, True, None, **kwargs)


def trace_rows(trace):
    return [json.loads(line) for line in trace.read_text().splitlines()] if trace.exists() else []


def test_fatto_lists_only_successful_triage_calls(profile):
    from assistant.recap import build_recap

    calls = [
        ToolCall("dwt", "mark_handled", {"thread": "#12"}, True,
                 json.dumps({"untrusted": {"from_name": "Colleague", "subject": "Fattura"}})),
        ToolCall("dwt", "snooze", {"thread": "#13", "until": "2026-11-13"}, False, "errore"),
        ToolCall("notes", "mark_handled", {"thread": "#99"}, True, ""),
        ToolCall("dwt", "search", {}, True, ""),
    ]
    export_turn(profile, calls=calls, sid="z_first")
    export_turn(profile, at=NOW - timedelta(hours=1), calls=[
        ToolCall("dwt", "snooze", {"thread": "c:later", "until": "2026-11-13"}, True, "{}"),
        ToolCall("dwt", "relabel", {"id": 15, "bucket": "clienti"}, True, '{"new":"clienti"}'),
        ToolCall("dwt", "mute_thread", {"thread": "#16"}, True, "{}"),
        ToolCall("dwt", "reopen", {"thread": "#17"}, True, "{}"),
    ], sid="a_later")
    for at, sid in ((START - timedelta(seconds=1), "old"), (NOW, "future")):
        export_turn(profile, at=at, calls=[ToolCall("dwt", "reopen", {"thread": "#999"}, True, "")], sid=sid)
    recap = build_recap(profile, START, NOW, client=MailState(), summarize=lambda turns: "Apertura.")
    fatto = recap.text.split("Fatto\n")[1].split("\n\n")[0]
    assert fatto.splitlines() == [
        "- chiusa · Colleague · Fattura", "- rimandata al 13 novembre · c:later",
        "- riclassificata come clienti · #15", "- silenziata · #16", "- riaperta · #17",
    ]


def test_sections_in_fixed_order(profile):
    from assistant.recap import build_recap

    export_turn(profile)
    recap = build_recap(profile, START, NOW, client=MailState(count=107), summarize=lambda turns: "Apertura.")
    titles = ["Fatto", "Ancora aperto", "Mi hai chiesto di ricordare", "Costo"]
    assert [line for line in recap.text.splitlines() if line in titles] == titles
    assert "Fatto\n- niente" in recap.text
    assert "- #1 · Mittente 1 · Oggetto 1 · 13/11/2026 09:00" in recap.text
    assert "- #5 ·" in recap.text and "- #6 ·" not in recap.text
    assert "- e altre 102" in recap.text


def test_no_activity_one_liner(profile):
    from assistant.recap import build_recap

    recap = build_recap(profile, START, NOW, client=MailState(count=3),
                        summarize=lambda turns: pytest.fail("summary without activity"))
    assert recap.text.startswith("Nessuna attività con l'assistente da mercoledì 11 novembre. Risposte ancora aperte: 3.\n")
    assert "\nCosto\n- Costo da 11/11/2026:" in recap.text
    assert "\nFatto\n" not in recap.text and not recap.activity


def test_notes_section_lists_notes_added_in_period(profile):
    from assistant.recap import build_recap

    notes = NotesStore(profile.memory_dir)
    notes.remember("Prima", START)
    note = notes.remember("A Colleague dai del tu", START + timedelta(seconds=1))
    notes.remember("Dopo", NOW)
    recap = build_recap(profile, START, NOW, client=MailState(count=0), summarize=lambda turns: "")
    assert recap.activity
    assert f"Mi hai chiesto di ricordare\n- «A Colleague dai del tu» (#{note.id})" in recap.text
    assert "Prima" not in recap.text and "Dopo" not in recap.text
    assert "Ancora aperto\n- niente" in recap.text


@pytest.mark.parametrize("minutes, expected", [(15, None), (16, "Posta aggiornata alle 09:44.")])
def test_stale_mail_line_when_freshness_over_15_min(profile, minutes, expected):
    from assistant.recap import build_recap, mail_freshness

    state = MailState(synced=NOW - timedelta(minutes=minutes))
    export_turn(profile)
    recap = build_recap(profile, START, NOW, client=state, summarize=lambda turns: "Apertura.")
    assert mail_freshness(state, NOW) == timedelta(minutes=minutes)
    assert recap.stale_note == expected
    if expected:
        assert recap.text.startswith(f"Apertura.\n{expected}\n")


def test_missing_sync_is_not_reported_as_fresh(profile):
    from assistant.recap import build_recap, mail_freshness

    state = MailState(synced=None)
    assert mail_freshness(state, NOW) > timedelta(minutes=15)
    recap = build_recap(profile, START, NOW, client=state, summarize=lambda turns: "")
    assert recap.stale_note == "Posta non ancora sincronizzata."


def test_deliver_writes_files_before_sending(profile, recap_dwt):
    from assistant.recap import deliver_recap

    recap = recap_value("è" * 3100)
    assert deliver_recap(profile, recap, NOW) == "sent"
    row, = trace_rows(recap_dwt[1])
    assert row["text"] == recap.text
    assert row["latest"] == "è" * 2999 + "…"
    assert row["stdin"] == ""
    assert row["argv"] == [
        "send-note", "--config", str(profile.settings.dwt_config), "--env", str(profile.settings.dwt_env),
        "--to", "dev@example.com", "--subject", recap.subject,
        "--file", str(profile.memory_dir / "recap/2026-11-12.md"),
    ]
    markers = load_markers(profile)
    assert markers.last_recap_day == date(2026, 11, 12)
    assert markers.last_recap_end == NOW and markers.recap_claim == "recap:2026-11-12"
    log = (profile.logs_dir / "recap.log").read_text()
    assert '"outcome": "sent"' in log
    assert "dev@example.com" not in log and recap.text not in log and "private diagnostic" not in log
    assert deliver_recap(profile, recap, NOW) == "skipped"
    assert len(trace_rows(recap_dwt[1])) == 1


def test_deliver_unknown_never_resends(profile, recap_dwt):
    from assistant.recap import deliver_recap

    recap_dwt[0].write_text('{"exit": 3}')
    assert deliver_recap(profile, recap_value(), NOW) == "unknown"
    assert deliver_recap(profile, recap_value(), NOW) == "skipped"
    assert len(trace_rows(recap_dwt[1])) == 1
    event, = pending_status(profile)
    assert event.key == "recap_unknown:2026-11-12"
    assert event.text == "Il recap del giovedì 12 novembre potrebbe non essere arrivato: controlla la posta."
    assert load_markers(profile).last_recap_end is None


def test_deliver_leftover_claim_never_resends(profile, recap_dwt, monkeypatch):
    from assistant import recap

    class Crash(BaseException):
        pass

    with monkeypatch.context() as patch:
        patch.setattr(recap.subprocess, "run", lambda *a, **kw: (_ for _ in ()).throw(Crash()))
        with pytest.raises(Crash):
            recap.deliver_recap(profile, recap_value(), NOW)
    assert load_markers(profile).recap_claim == "recap:2026-11-12"
    assert recap.deliver_recap(profile, recap_value(), NOW) == "skipped"
    assert trace_rows(recap_dwt[1]) == []


def test_deliver_failed_allows_retry(profile, recap_dwt):
    from assistant.recap import deliver_recap

    recap_dwt[0].write_text('{"exit": 4}')
    assert deliver_recap(profile, recap_value(), NOW) == "failed"
    markers = load_markers(profile)
    assert markers.recap_claim is None and markers.recap_claim_start is None and markers.recap_claim_end is None
    assert markers.last_recap_end is None and pending_status(profile) == []
    recap_dwt[0].write_text('{"exit": 0}')
    assert deliver_recap(profile, recap_value(), NOW) == "sent"
    assert len(trace_rows(recap_dwt[1])) == 2


@pytest.mark.parametrize("mode", ["signal", "exit1", "exit2", "timeout"])
def test_deliver_killed_process_is_unknown(profile, recap_dwt, monkeypatch, mode):
    from assistant import recap

    if mode == "signal":
        recap_dwt[0].write_text('{"signal": true}')
    elif mode.startswith("exit"):
        recap_dwt[0].write_text(json.dumps({"exit": int(mode[4:])}))
    elif mode == "timeout":
        recap_dwt[0].write_text('{"sleep": 30}')
        original = subprocess.run

        def short_timeout(*args, **kwargs):
            assert kwargs["timeout"] == 120 and kwargs["stdin"] == subprocess.DEVNULL
            return original(*args, **{**kwargs, "timeout": 0.2})

        monkeypatch.setattr(recap.subprocess, "run", short_timeout)
    assert recap.deliver_recap(profile, recap_value(), NOW) == "unknown"
    assert load_markers(profile).recap_claim == "recap:2026-11-12"
    assert [event.key for event in pending_status(profile)] == ["recap_unknown:2026-11-12"]
    assert recap.deliver_recap(profile, recap_value(), NOW) == "skipped"
    assert len(trace_rows(recap_dwt[1])) <= 1


@pytest.mark.parametrize("mode", ["missing", "not_executable"])
def test_deliver_spawn_failure_is_failed_and_retryable(profile, recap_dwt, fake_dwt, mode):
    from assistant.recap import deliver_recap

    failed_profile = profile
    if mode == "missing":
        failed_profile = replace(profile, settings=replace(profile.settings, dwt_bin="/missing/fake-dwt"))
    else:
        fake_dwt.path.chmod(0o600)
    assert deliver_recap(failed_profile, recap_value(), NOW) == "failed"
    markers = load_markers(profile)
    assert markers.recap_claim is None and markers.recap_claim_start is None and markers.recap_claim_end is None
    assert markers.last_recap_end is None and markers.last_recap_day is None
    assert pending_status(profile) == [] and trace_rows(recap_dwt[1]) == []
    log, = (profile.logs_dir / "recap.log").read_text().splitlines()
    assert json.loads(log)["outcome"] == "failed"
    fake_dwt.path.chmod(0o700)
    assert deliver_recap(profile, recap_value(), NOW) == "sent"
    assert len(trace_rows(recap_dwt[1])) == 1


def test_deliver_claim_freezes_period(profile, recap_dwt):
    from assistant.recap import deliver_recap

    recap_dwt[0].write_text('{"exit": 3}')
    assert deliver_recap(profile, recap_value(), NOW) == "unknown"
    markers = trace_rows(recap_dwt[1])[0]["markers"]
    assert markers["recap_claim"] == "recap:2026-11-12"
    assert datetime.fromisoformat(markers["recap_claim_start"]) == START
    assert datetime.fromisoformat(markers["recap_claim_end"]) == NOW
    later = NOW + timedelta(hours=1)
    assert deliver_recap(profile, replace(recap_value(), period_end=later), later) == "skipped"
    assert load_markers(profile).recap_claim_end == NOW


def test_deliver_claim_fsynced_before_send(profile, recap_dwt, monkeypatch):
    from assistant import recap

    syncs = []
    fsync, run = os.fsync, subprocess.run

    def observe_sync(fd):
        syncs.append("directory" if stat.S_ISDIR(os.fstat(fd).st_mode) else "file")
        fsync(fd)

    def observe_send(*args, **kwargs):
        assert syncs[-2:] == ["file", "directory"]
        assert load_markers(profile).recap_claim == "recap:2026-11-12"
        return run(*args, **kwargs)

    monkeypatch.setattr(os, "fsync", observe_sync)
    monkeypatch.setattr(recap.subprocess, "run", observe_send)
    assert recap.deliver_recap(profile, recap_value(), NOW) == "sent"


def test_recap_includes_and_acks_status(profile, recap_dwt):
    from assistant.recap import build_recap, deliver_recap

    record_status(profile, "promoted:1", "Ho imparato una regola.", START)
    recap = build_recap(profile, START, NOW, client=MailState(), summarize=lambda turns: "")
    assert "Nota: Ho imparato una regola." in recap.text
    record_status(profile, "job_failing:export", "Esportazione in ritardo.", NOW)
    recap_dwt[0].write_text('{"exit": 4}')
    assert deliver_recap(profile, recap, NOW) == "failed"
    assert len(pending_status(profile)) == 2
    recap_dwt[0].write_text('{"exit": 0}')
    assert deliver_recap(profile, recap, NOW) == "sent"
    assert [event.key for event in pending_status(profile)] == ["job_failing:export"]


@pytest.mark.parametrize("recipients", [[], ["a@example.com", "b@example.com"], "a@example.com", [""]])
def test_recipient_must_be_single(profile, recap_dwt, recipients):
    from assistant.recap import deliver_recap

    profile.settings.dwt_config.write_text(json.dumps({"digest_recipients": recipients}))
    assert deliver_recap(profile, recap_value(), NOW) == "failed"
    assert load_markers(profile).recap_claim is None
    assert trace_rows(recap_dwt[1]) == []
    assert (profile.memory_dir / "recap/2026-11-12.md").exists()
    assert "recipient" in (profile.logs_dir / "recap.log").read_text()


def test_summarizer_failure_does_not_block(profile):
    from assistant.recap import build_recap

    export_turn(profile, calls=[ToolCall("dwt", "mark_handled", {"thread": "#4"}, True, "")])

    def broken(turns):
        raise RuntimeError("summary unavailable")

    recap = build_recap(profile, START, NOW, client=MailState(), summarize=broken)
    assert recap.text.startswith("Fatto\n- chiusa · #4")


@pytest.mark.parametrize("dry_run", [True, False])
def test_cli_recap_dry_run_never_sends(profile, recap_dwt, monkeypatch, dry_run):
    from assistant import cli, recap

    monkeypatch.setattr(recap, "DwtClient", lambda *a, **kw: MailState())
    monkeypatch.setattr(recap, "summarize_with_agent", lambda p: lambda turns: "")
    save_markers(profile, Markers(last_recap_end=START, recap_claim="recap:previous"))
    saved = profile.markers_path.read_bytes()
    args = ["recap", "--profile", "dev", "--state-root", str(profile.root.parents[1])]
    if dry_run:
        args.append("--dry-run")
    assert cli.main(args) == 0
    assert (profile.memory_dir / "recap/latest.md").is_file()
    if dry_run:
        assert profile.markers_path.read_bytes() == saved
        assert trace_rows(recap_dwt[1]) == []
    else:
        assert len(trace_rows(recap_dwt[1])) == 1


def test_cli_recap_tick_lock_busy_and_released(profile, monkeypatch, capsys):
    from assistant.cli import main
    from assistant.locks import tick_lock
    from assistant import recap

    args = ["recap", "--profile", "dev", "--state-root", str(profile.root.parents[1]), "--dry-run"]
    monkeypatch.setattr(recap, "DwtClient", lambda *a, **kw: pytest.fail("busy lock must prevent building"))
    with tick_lock(profile) as acquired:
        assert acquired
        assert main(args) == 1
    assert "Un altro processo dell'assistente è in corso; riprova tra poco." in capsys.readouterr().err
    with pytest.raises(RuntimeError), tick_lock(profile) as acquired:
        assert acquired
        raise RuntimeError("release on failure")
    with tick_lock(profile) as acquired:
        assert acquired


def test_subject_italian_date(profile):
    from assistant.recap import build_recap

    # Already Friday in Rome; the UTC date is still Thursday.
    now = datetime(2026, 11, 12, 23, 30, tzinfo=timezone.utc)
    recap = build_recap(profile, START, now, client=MailState(), summarize=lambda turns: "")
    assert recap.subject == "Recap assistente — venerdì 13 novembre"


def summary_setup(profile, monkeypatch, *, code=0, exception=None, events=None):
    from assistant import opencode_env
    from assistant.sessions import turns_from_export

    skill = profile.root / "harness-live/skills/recap/SKILL.md"
    skill.parent.mkdir(parents=True)
    skill.write_text("RECAP_SKILL: al massimo tre frasi italiane.")
    export_turn(profile, calls=[
        ToolCall("dwt", "mark_handled", {"thread": "#10"}, True, "{}"),
        ToolCall("dwt", "snooze", {"thread": "#11"}, False, "SECRET_FAILED_OUTPUT"),
    ], reply="R" * 1000)
    turns = turns_from_export(profile.exports_dir / "ses_ui.json", "dev")
    text = FIXTURE.read_text() if events is None else events
    text = text.replace("I don't have the contents of `memory/notes.md` loaded in my context, so I can't state its always-loaded marker from memory.\\n\\nDONE",
                        "Prima frase. Seconda frase! Terza frase? Quarta frase.")
    calls = []

    def run(p, purpose, args, **kwargs):
        calls.append((purpose, args, kwargs))
        if exception:
            raise exception(text)
        return subprocess.CompletedProcess(args, code, text, "secret stderr")

    monkeypatch.setattr(opencode_env, "run_opencode", run)
    return turns, calls


def test_summary_is_tool_free_bounded_and_records_usage(profile, monkeypatch):
    from assistant.recap import summarize_with_agent

    turns, calls = summary_setup(profile, monkeypatch)
    assert summarize_with_agent(profile)(turns) == "Prima frase. Seconda frase! Terza frase?"
    purpose, args, kwargs = calls[0]
    assert purpose == "ops" and args[:5] == ["run", "--format", "json", "--model", profile.settings.model]
    prompt = args[-1]
    assert "RECAP_SKILL" in prompt and "Chiudi questa" in prompt and "mark_handled" in prompt
    assert "SECRET_FAILED_OUTPUT" not in prompt and "R" * 801 not in prompt
    assert "R" * 800 in prompt and "personale" in prompt and "costi" in prompt
    config = json.loads((kwargs["cwd"] / "opencode.json").read_text())
    assert config["mcp"]["servers"] == {}
    assert config["permissions"] == [{"action": "*", "resource": "*", "effect": "deny"}]
    assert kwargs["timeout"] == 300
    row, = [json.loads(line) for line in (profile.root / "usage.jsonl").read_text().splitlines()]
    assert row["call_id"].endswith(":ses_e32d1389bffeWkff0K3fth8DPG")
    assert row["call_id"].startswith("recap-summary:")
    assert (row["tokens_in"], row["tokens_out"], row["tokens_cache_read"]) == (1420, 77, 1536)


def test_summarize_handles_dash_leading_skill(profile, fake_opencode):
    from assistant.recap import summarize_with_agent
    from assistant.sessions import turns_from_export

    skill = profile.root / "harness-live/skills/recap/SKILL.md"
    skill.parent.mkdir(parents=True)
    skill.write_text("---\nname: recap\n---\nScrivi un'apertura in italiano.")
    export_turn(profile)
    turns = turns_from_export(profile.exports_dir / "ses_ui.json", "dev")
    fake_opencode.path.write_text(
        f"#!{sys.executable}\n"
        "import json, pathlib, sys\n"
        "args = sys.argv[1:]\n"
        "assert args[0] == 'run'\n"
        "options = args[1:args.index('--')] if '--' in args else args[1:]\n"
        "if any(arg.startswith('---') for arg in options): sys.exit(1)\n"
        f"pathlib.Path({str(fake_opencode.record)!r}).write_text(json.dumps(args))\n"
        "print(json.dumps({'type': 'text', 'part': {'text': 'Apertura italiana.'}}))\n"
    )
    assert summarize_with_agent(profile)(turns) == "Apertura italiana."
    argv = json.loads(fake_opencode.record.read_text())
    separator = argv.index("--")
    assert "--standalone" in argv[:separator]
    assert argv[separator + 1].startswith("---\n") and separator == len(argv) - 2


def test_transcript_capped_to_recent_turns(profile, monkeypatch):
    from assistant.recap import summarize_with_agent

    turns, calls = summary_setup(profile, monkeypatch)
    turns = [replace(turns[0], index=i, message=f"turno{i}:" + "x" * 20_000) for i in range(4)]
    assert summarize_with_agent(profile)(turns) == "Prima frase. Seconda frase! Terza frase?"
    transcript = calls[0][1][-1].rsplit("\n\n", 1)[1]
    assert len(transcript) <= 60_000
    assert [turn["utente"].split(":")[0] for turn in json.loads(transcript)] == ["turno2", "turno3"]
    assert all(len(turn["utente"]) == 20_007 for turn in json.loads(transcript))


def test_transcript_truncates_oversized_turn_fields(profile, monkeypatch):
    from assistant.recap import summarize_with_agent

    turns, calls = summary_setup(profile, monkeypatch)
    huge = 'é"\\\n' * 40_000
    latest = replace(turns[0], message="recente:" + huge, tool_calls=[
        ToolCall("dwt", "mark_handled", {"nested": [{"text": "argomento:" + huge}]},
                 True, "risultato:" + huge),
    ])
    assert summarize_with_agent(profile)([turns[0], latest]) == "Prima frase. Seconda frase! Terza frase?"
    transcript = calls[0][1][-1].rsplit("\n\n", 1)[1]
    assert len(transcript) <= 60_000
    turn, = json.loads(transcript)
    assert turn["utente"].startswith("recente:")
    action, = turn["azioni_riuscite"]
    assert action["risultato"].startswith("risultato:")
    assert action["argomenti"]["nested"][0]["text"].startswith("argomento:")
    assert latest.message == "recente:" + huge


@pytest.mark.parametrize("mode", ["nonzero", "timeout", "oserror", "malformed", "error_event"])
def test_summary_failures_return_empty_and_record_usage(profile, monkeypatch, mode):
    from assistant.recap import summarize_with_agent

    kwargs = {}
    if mode == "nonzero":
        kwargs["code"] = 1
    elif mode == "timeout":
        kwargs["exception"] = lambda text: subprocess.TimeoutExpired("opencode", 300, output=text.encode())
    elif mode == "oserror":
        kwargs["exception"] = lambda text: OSError("failed")
    elif mode == "malformed":
        kwargs["events"] = FIXTURE.read_text() + "not json\n"
    elif mode == "error_event":
        kwargs["events"] = FIXTURE.read_text() + '{"type":"error","error":{"message":"failed"}}\n'
    turns, _ = summary_setup(profile, monkeypatch, **kwargs)
    assert summarize_with_agent(profile)(turns) == ""
    rows = [json.loads(line) for line in (profile.root / "usage.jsonl").read_text().splitlines()]
    assert len(rows) == 1
    assert rows[0]["tokens_in"] == (0 if mode == "oserror" else 1420)
