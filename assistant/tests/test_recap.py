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
        if tool == "get_message":
            return {"id": args["id"], "untrusted": {"from_name": "Colleague", "from": "colleague@example.com",
                    "subject": "Fattura", "preview": "", "body": ""}}
        if tool == "context":
            return {"buckets": [{"key": "clienti", "title": "Clienti e preventivi"}]}
        assert tool == "needs_reply"
        offset = int((args or {}).get("cursor", 0))
        page = self.items[offset:offset + 100]
        cursor = str(offset + 100) if offset + 100 < len(self.items) else ""
        return {"items": page, "meta": {"next_cursor": cursor}}


def export_turn(profile, *, at=START, calls=(), message="Chiudi questa", reply="Ho chiuso.", sid="ses_ui",
                completed_after=timedelta(seconds=1)):
    profile.exports_dir.mkdir(exist_ok=True)
    content = [{"type": "text", "text": reply}]
    content += [
        {"type": "tool", "name": f"{call.server}_{call.tool}",
         "state": {"input": call.args, "status": "completed" if call.ok else "error",
                   "output": call.output}} for call in calls
    ]
    model = {"providerID": "azure-ai", "id": "DeepSeek-V4.1-Flash-1017"}
    reply_time = {"created": (at + timedelta(milliseconds=1)).timestamp() * 1000}
    if completed_after is not None:
        reply_time["completed"] = (at + completed_after).timestamp() * 1000
    (profile.exports_dir / f"{sid}.json").write_text(json.dumps({
        "info": {"id": sid, "model": model}, "messages": [
            {"id": "user", "type": "user", "text": message,
             "time": {"created": at.timestamp() * 1000}},
            {"id": "reply", "type": "assistant", "content": content, "model": model,
             "time": reply_time},
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
        "row = {'argv': args, 'env': dict(os.environ), 'text': path.read_text(), 'stdin': sys.stdin.read(),\n"
        "       'latest': (root/'memory/recap/latest.md').read_text(),\n"
        "       'markers': json.loads((root/'markers.json').read_text())}\n"
        f"with pathlib.Path({str(trace)!r}).open('a') as out: out.write(json.dumps(row)+'\\n')\n"
        "if len(path.read_bytes()) > 65536: sys.exit(4)\n"
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
        ToolCall("dwt", "mark_handled", {"thread": "c:invoice", "inbound_id": 12}, True,
                 '{"thread":"c:invoice","state":"handled","due_at":null,"snoozed_until":null}'),
        ToolCall("dwt", "snooze", {"thread": "c:failed", "inbound_id": 13, "until": "2026-11-13"}, False, "errore"),
        ToolCall("notes", "mark_handled", {"thread": "#99"}, True, ""),
        ToolCall("dwt", "search", {}, True, ""),
    ]
    export_turn(profile, calls=calls, sid="z_first")
    export_turn(profile, at=NOW - timedelta(hours=1), calls=[
        ToolCall("dwt", "snooze", {"thread": "c:invoice", "inbound_id": 12, "until": "2026-11-13"}, True,
                 '{"thread":"c:invoice","state":"snoozed","due_at":null,"snoozed_until":"2026-11-13T08:00:00+01:00"}'),
        ToolCall("dwt", "relabel", {"id": 15, "bucket": "clienti"}, True, '{"old":"altro","new":"clienti","state":"open"}'),
        ToolCall("dwt", "mute_thread", {"thread": "c:muted"}, True,
                 '{"thread":"c:muted","state":"muted","due_at":null,"snoozed_until":null}'),
        ToolCall("dwt", "reopen", {"thread": "#16"}, True,
                 '{"thread":"c:reopened","state":"open","due_at":null,"snoozed_until":null}'),
    ], sid="a_later")
    for at, sid in ((START - timedelta(seconds=2), "old"), (NOW, "future")):
        export_turn(profile, at=at, calls=[ToolCall("dwt", "reopen", {"thread": "#999"}, True, "")], sid=sid)
    mail = MailState()
    recap = build_recap(profile, START, NOW, client=mail, summarize=lambda turns: "Apertura.")
    fatto = recap.text.split("Fatto\n")[1].split("\n\n")[0]
    assert fatto.splitlines() == [
        "- chiusa: «Fattura» di Colleague (#12)", "- rimandata al 13 novembre: «Fattura» di Colleague (#12)",
        "- riclassificata come Clienti e preventivi: «Fattura» di Colleague (#15)",
        "- silenziata: c:muted", "- riaperta: «Fattura» di Colleague (#16)",
    ]
    assert [args for tool, args in mail.calls if tool == "get_message"] == [{"id": 12}, {"id": 15}, {"id": 16}]
    assert sum(tool == "context" for tool, _ in mail.calls) == 1


def test_fatto_lookup_failures_keep_ids_and_cache_errors(profile):
    from assistant.dwt_client import DwtError
    from assistant.recap import build_recap

    class UnavailableMail(MailState):
        def call(self, tool, args=None):
            if tool in ("get_message", "context"):
                self.calls.append((tool, args))
                raise DwtError("not found")
            return super().call(tool, args)

    export_turn(profile, calls=[
        ToolCall("dwt", "mark_handled", {"thread": "c:raw-key", "inbound_id": 12}, True,
                 '{"thread":"c:raw-key","state":"handled","due_at":null,"snoozed_until":null}'),
        ToolCall("dwt", "relabel", {"id": 12, "bucket": "clienti"}, True,
                 '{"old":"altro","new":"clienti","state":"open"}'),
    ])
    mail = UnavailableMail()
    recap = build_recap(profile, START, NOW, client=mail, summarize=lambda turns: "")
    assert "- chiusa: #12" in recap.text
    assert "- riclassificata come clienti: #12" in recap.text
    assert "c:raw-key" not in recap.text
    assert [args for tool, args in mail.calls if tool == "get_message"] == [{"id": 12}]


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


@pytest.mark.parametrize("completed_after", [timedelta(minutes=1), timedelta(minutes=2)])
def test_action_completed_after_boundary_counted_next_recap(profile, completed_after):
    from assistant.recap import build_recap
    from assistant.sessions import turns_from_export

    started = NOW - timedelta(minutes=1)
    calls = [ToolCall("dwt", "mark_handled", {"thread": "#across-boundary"}, True, "")]
    export_turn(profile, at=started, calls=calls, completed_after=None)
    first = build_recap(profile, START, NOW, client=MailState(),
                        summarize=lambda turns: pytest.fail("incomplete reply is not activity"))
    assert not first.activity
    assert "#across-boundary" not in first.text
    turn, = turns_from_export(profile.exports_dir / "ses_ui.json", "dev")
    assert turn.at == started and turn.completed_at is None

    export_turn(profile, at=started, calls=calls, completed_after=completed_after)
    completed, = turns_from_export(profile.exports_dir / "ses_ui.json", "dev")
    assert completed.at == started and completed.completed_at == started + completed_after
    # Completion exactly at NOW is excluded from the first period and included in the next.
    refreshed_first = build_recap(profile, START, NOW, client=MailState(),
                                  summarize=lambda turns: pytest.fail("completion is outside this period"))
    assert not refreshed_first.activity and "#across-boundary" not in refreshed_first.text
    second = build_recap(profile, NOW, NOW + timedelta(days=1), client=MailState(), summarize=lambda turns: "")
    assert second.activity and second.text.count("chiusa: #across-boundary") == 1
    third = build_recap(profile, NOW + timedelta(days=1), NOW + timedelta(days=2),
                        client=MailState(), summarize=lambda turns: "")
    assert not third.activity and "#across-boundary" not in third.text


def test_no_activity_one_liner(profile):
    from assistant.recap import build_recap

    recap = build_recap(profile, START, NOW, client=MailState(count=3),
                        summarize=lambda turns: pytest.fail("summary without activity"))
    assert recap.text.startswith("Nessuna attività con l'assistente da mercoledì 11 novembre. Risposte ancora aperte: 3.\n")
    assert "\nCosto\n- Costo da 11/11/2026:" in recap.text
    assert "\nFatto\n" not in recap.text and not recap.activity


def test_first_recap_includes_activity_since_install_marker(profile):
    from assistant.recap import build_recap, recap_period_start

    save_markers(profile, Markers(installed_at=START))
    export_turn(profile, at=START + timedelta(hours=1), calls=[
        ToolCall("dwt", "mark_handled", {"thread": "#since-install"}, True, ""),
    ])
    recap = build_recap(profile, recap_period_start(profile, NOW), NOW,
                        client=MailState(), summarize=lambda turns: "")
    assert recap.period_start == START
    assert "chiusa: #since-install" in recap.text


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
        "--state-dir", str(profile.dwt_state_dir),
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


def test_send_note_passes_state_dir(profile, recap_dwt, monkeypatch):
    from assistant.recap import deliver_recap

    monkeypatch.setenv("DWT_STATE_DIR", "/live/state")
    monkeypatch.setenv("DWT_CONFIG", "/live/config.json")
    monkeypatch.setenv("DWT_OTHER_OVERRIDE", "unsafe")
    monkeypatch.setenv("AZURE_API_KEY", "must-not-reach-send-note")
    monkeypatch.setenv("LANG", "it_IT.UTF-8")
    assert deliver_recap(profile, recap_value(), NOW) == "sent"
    row, = trace_rows(recap_dwt[1])
    assert "--state-dir" in row["argv"]
    assert row["argv"][row["argv"].index("--state-dir") + 1] == str(profile.dwt_state_dir)
    # Python may add LC_CTYPE at startup, independently of the supplied env.
    assert {key: value for key, value in row["env"].items() if key != "LC_CTYPE"} == {
        key: os.environ[key] for key in ("PATH", "HOME", "LANG")
    }


@pytest.mark.parametrize("source", ["actions", "opening", "bullets", "direct"])
def test_oversized_recap_is_trimmed_under_send_limit(profile, recap_dwt, source):
    from assistant.recap import build_recap, deliver_recap, write_recap

    calls = [ToolCall("dwt", "mark_handled", {"thread": f"azione{n}:" + "🙂" * 1000}, True, "")
             for n in range(70)]
    export_turn(profile, calls=calls)
    mail = MailState()
    opening = "🙂" * 30_000 if source == "opening" else "Apertura."
    if source == "bullets":
        NotesStore(profile.memory_dir).remember("è" * 500, START + timedelta(hours=1))
        mail.items[0]["untrusted"]["subject"] = "🙂" * 1000
    recap = (recap_value("🙂" * 30_000) if source == "direct" else
             build_recap(profile, START, NOW, client=mail, summarize=lambda turns: opening))
    if source != "direct":
        assert len(recap.text.encode("utf-8")) <= 60_000
    if source in ("actions", "bullets"):
        fatto = recap.text.split("Fatto\n")[1].split("\n\n")[0].splitlines()
        assert len(fatto) == 41
        assert fatto[-1] == "- e altre 30 azioni"
        assert all(len(line[2:]) <= 300 for line in recap.text.splitlines() if line.startswith("- "))
        assert "azione39:" in recap.text and "azione40:" not in recap.text
    path = write_recap(profile, recap, NOW)
    text = path.read_text()
    assert len(text.encode("utf-8")) <= 60_000
    if source in ("opening", "direct"):
        assert text.endswith("\n…(testo accorciato)\n")
    assert deliver_recap(profile, recap, NOW) == "sent"
    row, = trace_rows(recap_dwt[1])
    assert row["text"] == text


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
def test_legacy_recipient_requires_unambiguous_digest_default(profile, recap_dwt, recipients):
    from assistant.recap import deliver_recap

    profile.settings.dwt_config.write_text(json.dumps({"digest_recipients": recipients}))
    assert deliver_recap(profile, recap_value(), NOW) == "failed"
    assert load_markers(profile).recap_claim is None
    assert trace_rows(recap_dwt[1]) == []
    assert (profile.memory_dir / "recap/2026-11-12.md").exists()
    assert "recipient" in (profile.logs_dir / "recap.log").read_text()


def test_explicit_recap_recipient_with_multiple_digest_recipients(profile, recap_dwt):
    from assistant.recap import deliver_recap, recap_recipient

    profile.settings.dwt_config.write_text(json.dumps({
        "digest_recipients": ["dev@example.com", "other@example.com"],
    }))
    p = replace(profile, settings=replace(profile.settings, recap_recipient="DEV@example.com"))
    assert recap_recipient(p) == "DEV@example.com"
    assert deliver_recap(p, recap_value(), NOW) == "sent"
    row, = trace_rows(recap_dwt[1])
    assert row["argv"][row["argv"].index("--to") + 1] == "DEV@example.com"


def test_recap_recipient_rechecks_current_config_before_claim(profile, recap_dwt):
    from assistant.recap import deliver_recap, recap_recipient

    p = replace(profile, settings=replace(profile.settings, recap_recipient="dev@example.com"))
    assert recap_recipient(p) == "dev@example.com"
    p.settings.dwt_config.write_text('{"digest_recipients": ["other@example.com"]}')
    with pytest.raises(ValueError, match="recap_recipient"):
        recap_recipient(p)
    assert deliver_recap(p, recap_value(), NOW) == "failed"
    assert load_markers(p).recap_claim is None
    assert trace_rows(recap_dwt[1]) == []


@pytest.mark.parametrize("dry_run", [False, True])
def test_cli_invalid_recipient_prevents_export_and_paid_summary(profile, recap_dwt, monkeypatch, dry_run):
    from assistant import cli, recap, sessions

    profile.settings.dwt_config.write_text('{"digest_recipients": []}')
    monkeypatch.setattr(sessions, "export_new_sessions", lambda p: pytest.fail("export before validation"))
    monkeypatch.setattr(recap, "DwtClient", lambda *a, **kw: pytest.fail("build before validation"))
    monkeypatch.setattr(recap, "summarize_with_agent", lambda p: pytest.fail("paid work before validation"))
    args = ["recap", "--profile", "dev", "--state-root", str(profile.root.parents[1])]
    assert cli.main(args + (["--dry-run"] if dry_run else [])) == 1
    assert load_markers(profile).recap_claim is None
    assert trace_rows(recap_dwt[1]) == []


def test_summarizer_failure_does_not_block(profile):
    from assistant.recap import build_recap

    export_turn(profile, calls=[ToolCall("dwt", "mark_handled", {"thread": "#4"}, True, "")])

    def broken(turns):
        raise RuntimeError("summary unavailable")

    recap = build_recap(profile, START, NOW, client=MailState(), summarize=broken)
    assert recap.text.startswith("Fatto\n- chiusa: «Fattura» di Colleague (#4)")


@pytest.mark.parametrize("dry_run", [True, False])
def test_cli_recap_dry_run_never_sends(profile, recap_dwt, monkeypatch, dry_run):
    from assistant import cli, recap, sessions

    monkeypatch.setattr(sessions, "export_new_sessions", lambda p: [])
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


@pytest.mark.parametrize("dry_run", [False, True])
def test_manual_recap_deferred_when_export_fails(profile, recap_dwt, monkeypatch, dry_run):
    from assistant import cli, recap, sessions

    monkeypatch.setattr(recap, "DwtClient", lambda *a, **kw: MailState())
    monkeypatch.setattr(recap, "summarize_with_agent", lambda p: lambda turns: "")
    save_markers(profile, Markers(installed_at=START, last_recap_end=START))
    saved = profile.markers_path.read_bytes()

    def fail(p):
        raise RuntimeError("export offline")

    monkeypatch.setattr(sessions, "export_new_sessions", fail)
    args = ["recap", "--profile", "dev", "--state-root", str(profile.root.parents[1])]
    if dry_run:
        args.append("--dry-run")
    assert cli.main(args) == 1
    assert trace_rows(recap_dwt[1]) == []
    assert profile.markers_path.read_bytes() == saved
    assert not (profile.memory_dir / "recap/latest.md").exists()

    def fresh_export(p):
        export_turn(p, at=START + timedelta(hours=1), calls=[
            ToolCall("dwt", "mark_handled", {"thread": "#fresh"}, True, ""),
        ])

    monkeypatch.setattr(sessions, "export_new_sessions", fresh_export)
    assert cli.main(args) == 0
    assert "chiusa: #fresh" in (profile.memory_dir / "recap/latest.md").read_text()
    assert len(trace_rows(recap_dwt[1])) == (0 if dry_run else 1)


@pytest.mark.parametrize("same_day", [False, True])
def test_manual_recap_reconciles_claim_first(profile, recap_dwt, monkeypatch, same_day):
    from assistant import cli, recap, sessions
    from assistant.locks import tick_lock

    class Clock(datetime):
        @classmethod
        def now(cls, tz=None):
            return NOW

    monkeypatch.setattr(cli, "datetime", Clock)
    frozen = NOW - timedelta(hours=1) if same_day else START
    claim_day = "2026-11-12" if same_day else "2026-11-11"
    save_markers(profile, Markers(installed_at=START - timedelta(days=1),
                                  last_recap_end=START - timedelta(days=1),
                                  recap_claim=f"recap:{claim_day}",
                                  recap_claim_start=START - timedelta(days=1), recap_claim_end=frozen))
    export_turn(profile, at=START - timedelta(hours=1), sid="old", calls=[
        ToolCall("dwt", "mark_handled", {"thread": "#already-claimed"}, True, ""),
    ])
    export_turn(profile, at=START + timedelta(hours=1), sid="new", calls=[
        ToolCall("dwt", "mark_handled", {"thread": "#new"}, True, ""),
    ])
    recovered = []

    def export(p):
        with tick_lock(p) as acquired:
            assert not acquired
        recovered.append(load_markers(p))
        return []

    monkeypatch.setattr(sessions, "export_new_sessions", export)
    monkeypatch.setattr(recap, "DwtClient", lambda *a, **kw: MailState())
    monkeypatch.setattr(recap, "summarize_with_agent", lambda p: lambda turns: "")
    args = ["recap", "--profile", "dev", "--state-root", str(profile.root.parents[1])]
    assert cli.main(args) == 0
    markers = load_markers(profile)
    if same_day:
        assert trace_rows(recap_dwt[1]) == [] and recovered == []
        assert markers.last_recap_end == frozen and markers.recap_claim == "recap:2026-11-12"
        assert [event.key for event in pending_status(profile)] == ["recap_unknown:2026-11-12"]
    else:
        row, = trace_rows(recap_dwt[1])
        snapshot, = recovered
        assert snapshot.last_recap_day == date(2026, 11, 11)
        assert snapshot.last_recap_end == START and snapshot.recap_claim == "recap:2026-11-11"
        assert row["markers"]["recap_claim_start"] == START.isoformat()
        assert "#already-claimed" not in row["text"] and "chiusa: #new" in row["text"]
        assert "Il recap del mercoledì 11 novembre potrebbe non essere arrivato: controlla la posta." in row["text"]
        assert markers.last_recap_end == NOW
    assert markers.last_recap_day == date(2026, 11, 12)


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
