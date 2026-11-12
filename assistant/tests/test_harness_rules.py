from pathlib import Path

import pytest

from assistant.h2 import harness2
from assistant.harness_rules import HARNESS2_STUB_PATHS, REQUIRED_GUARDRAIL_LINES, load_tree, validate_tree


harness_tree = harness2().tree
OPENCODE_SURFACE = harness2().OPENCODE_SURFACE
HARNESS_ROOT = Path(__file__).resolve().parents[1] / "harness"


@pytest.fixture
def tree():
    return load_tree(HARNESS_ROOT)


def test_live_tree_passes():
    tree = load_tree(HARNESS_ROOT)
    assert validate_tree(tree) is None
    harness_tree.validate(OPENCODE_SURFACE, tree, allow_subagents=False)


@pytest.mark.parametrize("line", REQUIRED_GUARDRAIL_LINES)
def test_missing_guardrail_line_rejected(tree, line):
    tree["guardrails.md"] = tree["guardrails.md"].replace(line, "")

    with pytest.raises(ValueError) as error:
        validate_tree(tree)

    assert "guardrails.md" in str(error.value)
    assert line in str(error.value)


@pytest.mark.parametrize("path, instruction", [
    ("skills/triage/SKILL.md", "send_mail"),
    ("systemprompt.md", "sendmail"),
    ("guardrails.md", "send_reply"),
    ("LongTermMEMORY.md", "sendreply"),
    ("tool_descriptions/tool_guidance.md", "sendMail"),
    ("skills/recap/SKILL.md", "SEND_REPLY"),
])
def test_sending_instruction_rejected(tree, path, instruction):
    tree[path] += f"\nUsa {instruction}.\n"

    with pytest.raises(ValueError) as error:
        validate_tree(tree)

    assert path in str(error.value)
    assert instruction in str(error.value)


@pytest.mark.parametrize("size", [16_000, 16_001])
def test_always_on_cap(tree, size):
    current_size = len(harness_tree.always_on_text(OPENCODE_SURFACE, tree))
    tree["LongTermMEMORY.md"] += "è" * (size - current_size)

    if size == 16_000:
        validate_tree(tree)
    else:
        with pytest.raises(ValueError) as error:
            validate_tree(tree)
        assert "always-on" in str(error.value)
        assert "16001" in str(error.value)
        assert "16000" in str(error.value)


@pytest.mark.parametrize("path", [
    "plugin/x.ts",
    "scripts/x.py",
    "skills/triage/scripts/x.py",
    "sub_agents/a/AGENT.md",
    "notes.txt",
    "other.md",
    "tool_descriptions/other.md",
    "skills/triage/notes.md",
    "skills/Triage/SKILL.md",
    "skills/tri_age/SKILL.md",
    "skills/triage/nested/SKILL.md",
    "../systemprompt.md",
    "/systemprompt.md",
])
def test_executable_surface_rejected(tree, path):
    tree[path] = "Unexpected file."

    with pytest.raises(ValueError) as error:
        validate_tree(tree)

    assert path in str(error.value)


@pytest.mark.parametrize("path", [
    "plugin/extra.md", "plugin/x.ts", "plugin/nested/README.md", "plugin/readme.md",
    "scripts/extra.md", "sub_agents/extra.md", "skills/triage/extra.py",
])
def test_other_plugin_files_still_rejected(tree, path):
    tree.update(dict.fromkeys(HARNESS2_STUB_PATHS, "send_mail"))
    tree[path] = "Not a placeholder."
    with pytest.raises(ValueError) as error:
        validate_tree(tree)
    assert path in str(error.value)


def test_problems_are_all_listed(tree):
    missing_line = REQUIRED_GUARDRAIL_LINES[0]
    tree["guardrails.md"] = tree["guardrails.md"].replace(missing_line, "")
    tree["notes.txt"] = "Unexpected file."

    with pytest.raises(ValueError) as error:
        validate_tree(tree)

    message = str(error.value)
    assert "guardrails.md" in message
    assert missing_line in message
    assert "notes.txt" in message
    assert " | " in message


def test_harness2_errors_are_included(tree):
    tree["skills/triage/SKILL.md"] = "---\nname: wrong\n---\n"
    tree["notes.txt"] = "Unexpected file."
    with pytest.raises(ValueError) as upstream_error:
        harness_tree.validate(OPENCODE_SURFACE, tree, allow_subagents=False)

    with pytest.raises(ValueError) as error:
        validate_tree(tree)

    message = str(error.value)
    assert str(upstream_error.value) in message
    assert "description:" in message
    assert "name: triage" in message
    assert "notes.txt" in message


def test_load_tree_includes_every_file(tmp_path):
    root = tmp_path / "harness"
    for path, content in {
        "systemprompt.md": b"Prompt in italiano.",
        "skills/triage/scripts/x.py": b"print('ciao')",
        ".hidden.md": b"Hidden file.",
        "__pycache__/x.pyc": b"\xff\x00",
    }.items():
        file = root / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_bytes(content)

    tree = load_tree(root)

    assert tree == {
        "systemprompt.md": "Prompt in italiano.",
        "skills/triage/scripts/x.py": "print('ciao')",
        ".hidden.md": "Hidden file.",
        "__pycache__/x.pyc": "\ufffd\x00",
    }
    with pytest.raises(ValueError) as error:
        validate_tree(tree)
    for path in ("skills/triage/scripts/x.py", ".hidden.md", "__pycache__/x.pyc"):
        assert path in str(error.value)
