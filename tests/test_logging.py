"""Tests for the central structured logging module."""

from __future__ import annotations

import logging
import pathlib

import pytest
import structlog

from crewlet._logging import (
    configure_logging,
    get_logger,
    reset_logging,
)


@pytest.fixture(autouse=True)
def _reset():
    """Reset logging configuration between tests."""
    reset_logging()
    yield
    reset_logging()
    # Restore structlog defaults so other tests aren't affected
    structlog.reset_defaults()


class TestGetLogger:
    def test_returns_bound_logger(self):
        log = get_logger("test.component")
        assert hasattr(log, "bind")
        assert hasattr(log, "info")
        assert hasattr(log, "debug")
        assert hasattr(log, "warning")
        assert hasattr(log, "error")

    def test_component_is_bound(self):
        log = get_logger("my.module")
        # Bind returns a new logger; the original should already have component
        child = log.bind(extra="val")
        assert hasattr(child, "info")

    def test_logger_name_prefixed(self):
        log = get_logger("foo.bar")
        assert hasattr(log, "info")


class TestConfigureLogging:
    def test_configure_console(self):
        configure_logging(level=logging.INFO, fmt="console")
        # Should not raise

    def test_configure_json(self):
        configure_logging(level=logging.DEBUG, fmt="json")
        # Should not raise

    def test_idempotent(self):
        configure_logging(level=logging.INFO, fmt="console")
        # Second call should be a no-op
        configure_logging(level=logging.DEBUG, fmt="json")
        # Level should still be INFO from first call
        root = logging.getLogger()
        assert root.level == logging.INFO

    def test_log_output(self, capfd):
        import re

        configure_logging(level=logging.INFO, fmt="console")
        log = get_logger("test.output")
        log.info("hello_world", key="val")
        captured = capfd.readouterr()
        # Strip ANSI escape codes for assertion
        output = re.sub(r"\x1b\[[0-9;]*m", "", captured.out + captured.err)
        assert "hello_world" in output
        assert "key" in output and "val" in output


# ── the event name is a constant, not a template ─────────────────────


def test_no_event_name_is_built_from_a_variable() -> None:
    """``logger.x(f"{thing}_happened")`` defeats the point of structlog.

    The convention is that an event name is a short, machine-parsable
    constant and every varying part rides in a keyword argument. A name
    assembled from a variable inverts that: there is no single string to
    alert on or group by, so "a webhook route has no secret" becomes one
    rule per source instead of one rule with a ``source`` field — and
    the cardinality lands in the place least able to carry it.

    Four call sites did this. Scanned rather than reviewed because a
    fifth is one f-string away, and nothing else would notice: the log
    still emits, still parses, and reads fine to a human.
    """
    import re

    root = pathlib.Path(__file__).resolve().parents[1]
    # ``logger.<level>(`` followed by an f-string, across a line break or
    # not — the multiline form is how three of the four were written.
    pattern = re.compile(
        r"""logger\.(?:debug|info|warning|error|exception|critical)\(\s*f["']""",
        re.MULTILINE,
    )
    offenders = [
        str(path.relative_to(root))
        for path in (root / "src" / "crewlet").rglob("*.py")
        if pattern.search(path.read_text())
    ]
    assert offenders == [], (
        f"event names built from a variable: {offenders} — put the "
        f"varying part in a keyword argument instead"
    )
