"""Public benchmark summaries contain metadata, never private conversation text."""

from assistant.bench.judge import RUBRIC_DIMENSIONS, TurnScore
from assistant.spend import SpendTotal


def test_report_has_no_reply_text(tmp_path):
    from assistant.bench.report import write_report

    score = TurnScore("seed-1", dict.fromkeys(RUBRIC_DIMENSIONS, 8.0), ["missing_action"],
                      8.0, "SECRET MAIL AND REPLY")
    path = write_report(tmp_path / "run", scores=[score], passed=False,
                        splits={"seed-1": "holdout"}, skipped=["seed-2"],
                        cost=SpendTotal(0.25, 123), reply="SECRET REPLY", mail="SECRET MAIL")
    assert path == tmp_path / "run/report.md"
    text = path.read_text()
    assert "FAIL" in text and "8.00" in text and "holdout" in text
    assert all(dim in text for dim in RUBRIC_DIMENSIONS)
    assert "seed-1" in text and "missing_action" in text and "seed-2" in text
    assert "non disponibile" in text and "123" in text and "0.25" in text
    assert "SECRET" not in text
    (path.parent / "reply.md").write_text("PRIVATE REPLY ARTIFACT")
    text = write_report(path.parent, scores=[score], passed=True, cost=SpendTotal(1.25)).read_text()
    assert "PASS" in text and "$1.25" in text and "PRIVATE" not in text


def test_incomplete_report_fails_even_with_good_partial_scores(tmp_path):
    from assistant.bench.report import write_report

    score = TurnScore("a", dict.fromkeys(RUBRIC_DIMENSIONS, 10.0), [], 10.0, "")
    text = write_report(tmp_path, scores=[score], passed=True, incomplete=True).read_text()
    assert "FAIL" in text and "incompleta" in text
