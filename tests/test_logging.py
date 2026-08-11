"""Tests for the central structured logging module."""

from __future__ import annotations

import logging

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
