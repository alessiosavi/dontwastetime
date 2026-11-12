from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import subprocess
import sys
from threading import Barrier
from zoneinfo import ZoneInfo

import anyio
from mcp.shared.memory import create_connected_server_and_client_session
import pytest

from assistant.notes_mcp import Note, NotesFull, NotesStore, create_server


NOW = datetime(2026, 11, 12, 10, tzinfo=ZoneInfo("Europe/Rome"))


def snapshot(directory):
    return {p.name: p.read_bytes() for p in directory.iterdir() if p.is_file()}


def test_remember_appends_and_renders(tmp_path):
    store = NotesStore(tmp_path)
    note = store.remember("  a Colleague dai del tu  ", NOW)
    second = store.remember("Fatture ogni lunedì", NOW + timedelta(minutes=1))

    assert note == Note(id=1, at=NOW, text="a Colleague dai del tu")
    assert second.id == 2
    assert (tmp_path / "notes.md").read_text() == (
        "- [2026-11-12] a Colleague dai del tu (#1)\n"
        "- [2026-11-12] Fatture ogni lunedì (#2)\n"
    )
    records = [json.loads(line) for line in (tmp_path / "notes.jsonl").read_text().splitlines()]
    assert records == [
        {"id": 1, "at": NOW.isoformat(), "text": "a Colleague dai del tu"},
        {"id": 2, "at": second.at.isoformat(), "text": "Fatture ogni lunedì"},
    ]
    assert NotesStore(tmp_path).list() == [note, second]


def test_forget_removes_and_rerenders(tmp_path):
    store = NotesStore(tmp_path)
    first = store.remember("Prima", NOW)
    second = store.remember("Seconda", NOW)

    assert store.forget(first.id) is True
    assert store.list() == [second]
    assert (tmp_path / "notes.md").read_text() == "- [2026-11-12] Seconda (#2)\n"
    records = [json.loads(line) for line in (tmp_path / "notes.jsonl").read_text().splitlines()]
    assert [record["id"] for record in records] == [2]
    before = snapshot(tmp_path)
    assert store.forget(999) is False
    assert snapshot(tmp_path) == before
    assert store.forget(second.id) is True
    assert NotesStore(tmp_path).list() == []
    assert (tmp_path / "notes.jsonl").read_text() == ""
    assert (tmp_path / "notes.md").read_text() == ""


def test_full_notes_refused(tmp_path):
    store = NotesStore(tmp_path)
    # Eight 21-character wrappers plus 3,832 text characters = exactly 4,000.
    for _ in range(7):
        store.remember("è" * 500, NOW)
    store.remember("è" * 332, NOW)
    assert len((tmp_path / "notes.md").read_text()) == 4000
    before = snapshot(tmp_path)
    notes = store.list()

    with pytest.raises(NotesFull):
        store.remember("Ancora", NOW)

    assert snapshot(tmp_path) == before
    assert store.list() == notes
    assert store.forget(8) is True
    assert store.remember("Spazio libero", NOW).id == 9


def test_added_since_filters_by_time(tmp_path):
    store = NotesStore(tmp_path)
    store.remember("Prima", NOW - timedelta(seconds=1))
    store.remember("Al confine", NOW)
    later = store.remember("Dopo", NOW + timedelta(seconds=1))
    cutoff = NOW.astimezone(timezone.utc)

    assert store.added_since(cutoff) == [later]
    store.forget(later.id)
    assert NotesStore(tmp_path).added_since(cutoff) == []


def test_ids_never_reused_after_forget(tmp_path):
    first = NotesStore(tmp_path).remember("Prima", NOW)
    assert NotesStore(tmp_path).forget(first.id) is True
    second = NotesStore(tmp_path).remember("Seconda", NOW)
    assert second.id == 2
    assert NotesStore(tmp_path).forget(second.id) is True
    assert NotesStore(tmp_path).remember("Terza", NOW).id == 3


@pytest.mark.parametrize("shared_store", [False, True])
def test_concurrent_remember_keeps_both(tmp_path, shared_store):
    first = NotesStore(tmp_path)
    second = first if shared_store else NotesStore(tmp_path)
    barrier = Barrier(2, timeout=10)

    def write_notes(store, prefix):
        ids = []
        for i in range(10):
            barrier.wait()
            ids.append(store.remember(f"{prefix} {i}", NOW).id)
        return ids

    with ThreadPoolExecutor(max_workers=2) as executor:
        a = executor.submit(write_notes, first, "prima sessione")
        b = executor.submit(write_notes, second, "seconda sessione")
        ids = a.result(timeout=20) + b.result(timeout=20)

    notes = NotesStore(tmp_path).list()
    assert sorted(ids) == list(range(1, 21))
    assert {note.text for note in notes} == {
        f"{prefix} {i}" for prefix in ("prima sessione", "seconda sessione") for i in range(10)
    }
    rendered = (tmp_path / "notes.md").read_text()
    assert len(rendered.splitlines()) == 20
    assert all(f"{note.text} (#{note.id})" in rendered for note in notes)


@pytest.mark.parametrize("text", ["", " \n\t ", "è" * 501])
def test_remember_rejects_empty_and_too_long(tmp_path, text):
    store = NotesStore(tmp_path)
    before = snapshot(tmp_path)
    with pytest.raises(ValueError):
        store.remember(text, NOW)
    assert snapshot(tmp_path) == before
    assert store.list() == []
    assert store.remember("Valida", NOW).id == 1


def test_remember_collapses_internal_whitespace(tmp_path):
    note = NotesStore(tmp_path).remember("  a Colleague\r\n dai\t del   tu  ", NOW)
    assert note.text == "a Colleague dai del tu"
    assert (tmp_path / "notes.md").read_text() == "- [2026-11-12] a Colleague dai del tu (#1)\n"


@pytest.mark.parametrize("at, date", [
    (datetime(2026, 11, 11, 23, 30, tzinfo=timezone.utc), "2026-11-12"),
    (datetime(2026, 7, 11, 22, 30, tzinfo=timezone.utc), "2026-07-12"),
])
def test_render_uses_rome_calendar_date(tmp_path, at, date):
    NotesStore(tmp_path).remember("Promemoria", at)
    assert (tmp_path / "notes.md").read_text() == f"- [{date}] Promemoria (#1)\n"


def test_naive_timestamps_rejected(tmp_path):
    store = NotesStore(tmp_path)
    with pytest.raises(ValueError):
        store.remember("Valida", datetime(2026, 11, 12))
    with pytest.raises(ValueError):
        store.added_since(datetime(2026, 11, 12))


def test_startup_repairs_markdown_from_jsonl(tmp_path):
    note = NotesStore(tmp_path).remember("Ricorda", NOW)
    (tmp_path / "notes.md").write_text("Rendering interrotto")
    assert NotesStore(tmp_path).list() == [note]
    assert (tmp_path / "notes.md").read_text() == "- [2026-11-12] Ricorda (#1)\n"
    (tmp_path / "notes.md").unlink()
    NotesStore(tmp_path)
    assert (tmp_path / "notes.md").read_text() == "- [2026-11-12] Ricorda (#1)\n"


def test_failed_replace_preserves_source_and_never_reuses_reserved_id(tmp_path, monkeypatch):
    import assistant.notes_mcp as module

    store = NotesStore(tmp_path)
    first = store.remember("Prima", NOW)
    source = (tmp_path / "notes.jsonl").read_bytes()
    rendered = (tmp_path / "notes.md").read_bytes()
    original_replace = module.os.replace

    def interrupted_replace(src, dst):
        assert Path(src).parent == tmp_path
        if Path(dst).name == "notes.jsonl":
            raise OSError("simulated interrupted replace")
        original_replace(src, dst)

    with monkeypatch.context() as patch:
        patch.setattr(module.os, "replace", interrupted_replace)
        with pytest.raises(OSError, match="interrupted replace"):
            store.remember("Interrotta", NOW)

    assert (tmp_path / "notes.jsonl").read_bytes() == source
    assert (tmp_path / "notes.md").read_bytes() == rendered
    assert store.list() == [first]
    assert NotesStore(tmp_path).remember("Terza", NOW).id == 3
    assert not list(tmp_path.glob("*.tmp"))


def test_mcp_server_lists_three_tools(tmp_path):
    async def exercise():
        server = create_server(tmp_path)
        assert server.name == "notes"
        with anyio.fail_after(10):
            async with create_connected_server_and_client_session(server) as client:
                tools = await client.list_tools()
                assert {tool.name for tool in tools.tools} == {"remember", "forget", "list_notes"}
                empty = await client.call_tool("list_notes", {})
                assert not empty.isError
                assert empty.content[0].text == "Non ho ancora note."
                saved = await client.call_tool("remember", {"text": "a Colleague dai del tu"})
                assert not saved.isError
                assert saved.content[0].text == (
                    "Ho salvato: a Colleague dai del tu (#1). Per toglierla: «dimentica la nota 1»."
                )
                listed = await client.call_tool("list_notes", {})
                assert not listed.isError
                assert listed.content[0].text == (tmp_path / "notes.md").read_text()
                forgotten = await client.call_tool("forget", {"note_id": 1})
                assert forgotten.content[0].text == "Ho tolto la nota 1."
                missing = await client.call_tool("forget", {"note_id": 1})
                assert missing.content[0].text == "Non trovo la nota 1."

    anyio.run(exercise)
    assert NotesStore(tmp_path).list() == []


def test_mcp_full_notes_returns_italian_message(tmp_path):
    store = NotesStore(tmp_path)
    for _ in range(7):
        store.remember("a" * 500, NOW)
    store.remember("a" * 332, NOW)
    before = snapshot(tmp_path)

    async def exercise():
        with anyio.fail_after(10):
            async with create_connected_server_and_client_session(create_server(tmp_path)) as client:
                result = await client.call_tool("remember", {"text": "Ancora"})
                assert not result.isError
                assert result.content[0].text == (
                    "Le note sono piene (massimo 4.000 caratteri): dimmi quale togliere."
                )

    anyio.run(exercise)
    assert snapshot(tmp_path) == before


def test_module_entry_point_exits_on_eof(tmp_path):
    result = subprocess.run(
        [sys.executable, "-m", "assistant", "notes-mcp", "--memory-dir", str(tmp_path / "memory")],
        input="", text=True, capture_output=True, timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert result.stdout == ""
    assert (tmp_path / "memory" / "notes.md").read_text() == ""
