"""Incremental UI exports and turns use the captured OpenCode 2.2.7 shape."""

from copy import deepcopy
from datetime import datetime, timezone
import json
from pathlib import Path
import sys

import pytest


FIXTURE = Path(__file__).parent / "fixtures/session_export.json"


@pytest.fixture
def session_opencode(profile, fake_opencode):
    """A subprocess fake chooses records from its actual XDG store."""
    trace = profile.root / "commands.jsonl"
    for store, ids in ((profile.opencode_home, ["ses_one", "ses_two"]),
                       (profile.root / "opencode-ops", ["ses_private_ops"])):
        data = store / "data"
        data.mkdir(parents=True)
        (data / "sessions.json").write_text(json.dumps([
            {"id": sid, "updated": 1, "created": 1, "projectId": "fixture",
             "directory": str(profile.workspace)} for sid in ids
        ]))
    fake_opencode.path.write_text(
        f"#!{sys.executable}\n"
        "import json, os, pathlib, sys\n"
        "args = sys.argv[1:]\n"
        f"with pathlib.Path({str(trace)!r}).open('a') as out: out.write(json.dumps(args)+'\\n')\n"
        "assert '--standalone' in args\n"
        "store = pathlib.Path(os.environ['XDG_DATA_HOME'])\n"
        "rows = json.loads((store / 'sessions.json').read_text())\n"
        "if args[:2] == ['session', 'list']:\n"
        "    assert args[2:6] == ['--format', 'json', '--max-count', '2147483647']\n"
        "    print(json.dumps(rows))\n"
        "elif args[:2] == ['session', 'export']:\n"
        "    if (store / 'fail').exists(): sys.exit(7)\n"
        "    row = next(r for r in rows if r['id'] == args[2])\n"
        "    print(json.dumps({'info':{'id':row['id'],'time':{'updated':row['updated']}},'messages':[]}))\n"
        "else: sys.exit(9)\n"
    )
    return trace


def test_export_is_incremental(profile, session_opencode):
    from assistant.sessions import export_new_sessions

    assert [p.name for p in export_new_sessions(profile)] == ["ses_one.json", "ses_two.json"]
    index = profile.exports_dir / "index.json"
    assert json.loads(index.read_text()) == {"ses_one": 1, "ses_two": 1}
    assert export_new_sessions(profile) == []
    listing = profile.opencode_home / "data/sessions.json"
    rows = json.loads(listing.read_text())
    rows[0]["updated"] = 2
    listing.write_text(json.dumps(rows))
    assert [p.name for p in export_new_sessions(profile)] == ["ses_one.json"]
    assert json.loads(index.read_text()) == {"ses_one": 2, "ses_two": 1}
    commands = [json.loads(line) for line in session_opencode.read_text().splitlines()]
    assert sum(command[:2] == ["session", "export"] for command in commands) == 3
    assert not list(profile.exports_dir.glob(".*"))


def test_export_reads_only_ui_store(profile, session_opencode):
    from assistant.sessions import export_new_sessions

    paths = export_new_sessions(profile)
    assert {p.stem for p in paths} == {"ses_one", "ses_two"}
    assert not (profile.exports_dir / "ses_private_ops.json").exists()


def test_failed_export_preserves_index_and_previous_export(profile, session_opencode):
    from assistant.sessions import export_new_sessions

    export_new_sessions(profile)
    saved = (profile.exports_dir / "index.json").read_bytes()
    old_export = (profile.exports_dir / "ses_one.json").read_bytes()
    listing = profile.opencode_home / "data/sessions.json"
    listing.write_text(listing.read_text().replace('"updated": 1', '"updated": 2'))
    (listing.parent / "fail").touch()
    with pytest.raises(RuntimeError):
        export_new_sessions(profile)
    assert (profile.exports_dir / "index.json").read_bytes() == saved
    assert (profile.exports_dir / "ses_one.json").read_bytes() == old_export


def test_export_cli(profile, session_opencode, capsys):
    from assistant.cli import main

    assert main(["export", "--profile", "dev", "--state-root", str(profile.root.parents[1])]) == 0
    assert len(capsys.readouterr().out.splitlines()) == 1
    assert (profile.exports_dir / "ses_two.json").exists()


def test_turns_from_fixture():
    from assistant.sessions import turns_from_export

    turns = turns_from_export(FIXTURE, "dev")
    assert len(turns) == 2
    first, second = turns
    assert first.turn_id == "ses_e32d1389bffeWkff0K3fth8DPG:0"
    assert first.at == datetime.fromtimestamp(1794443757.693, timezone.utc)
    assert first.history == [] and first.profile == "dev"
    assert len(first.tool_calls) == 1
    call = first.tool_calls[0]
    assert (call.server, call.tool, call.ok, call.output) == (
        "notes", "echo_note", True, "NOTE:synthetic-task1",
    )
    assert call.args == {"text": "synthetic-task1"}
    assert first.next_message == second.message
    assert second.reply == "SECOND_TURN_BLOCKED"
    assert second.history == [("user", first.message), ("assistant", first.reply)]
    assert second.next_message is None
    assert "reasoning_content" not in first.reply


def test_tool_name_split(tmp_path):
    from assistant.sessions import turns_from_export

    data = deepcopy(json.loads(FIXTURE.read_text()))
    tool = data["messages"][1]["content"][1]
    tool["name"] = "dwt_mark_handled"
    tool["state"]["content"][0]["text"] = "x" * 5000
    next_tool = deepcopy(tool)
    failed = deepcopy(tool)
    failed["state"]["error"] = "refused"
    other = deepcopy(tool)
    other["name"] = "notes_mark_handled"
    data["messages"][5]["content"].extend([next_tool, failed, other])
    path = tmp_path / "turns.json"
    path.write_text(json.dumps(data))
    first, second = turns_from_export(path, "operator")
    call, = first.tool_calls
    assert (call.server, call.tool) == ("dwt", "mark_handled")
    assert len(call.output) == 4000
    assert first.following_tool_calls == [second.tool_calls[0]]
    assert not second.tool_calls[1].ok
    assert second.following_tool_calls == []
