"""Tests for ServerStderrRelay — structured stdio-server stderr capture."""

import logging
import os

import pytest

from crewlet.mcp._stderr import _TAIL_LINES, ServerStderrRelay


@pytest.mark.asyncio
async def test_lines_become_debug_events(caplog):
    with caplog.at_level(logging.DEBUG, logger="crewlet.mcp.stderr"):
        async with ServerStderrRelay("plane") as relay:
            relay.errlog.write("WARNING:root:Failed to validate request\n")
            relay.errlog.write("second line\n")
            relay.errlog.flush()
        # __aexit__ closed the write end -> reader drained to EOF.

    assert "server_stderr" in caplog.text
    assert "Failed to validate request" in caplog.text
    assert "second line" in caplog.text
    assert relay.tail() == [
        "WARNING:root:Failed to validate request",
        "second line",
    ]


@pytest.mark.asyncio
async def test_blank_lines_dropped():
    async with ServerStderrRelay("plane") as relay:
        relay.errlog.write("\n\nreal\n\n")
        relay.errlog.flush()
    assert relay.tail() == ["real"]


@pytest.mark.asyncio
async def test_tail_is_bounded():
    async with ServerStderrRelay("noisy") as relay:
        for i in range(_TAIL_LINES + 25):
            relay.errlog.write(f"line-{i}\n")
        relay.errlog.flush()

    tail = relay.tail()
    assert len(tail) == _TAIL_LINES
    # Oldest lines fell off; the last line survived.
    assert tail[-1] == f"line-{_TAIL_LINES + 24}"
    assert tail[0] == "line-25"


@pytest.mark.asyncio
async def test_errlog_is_a_real_pipe():
    """The SDK passes errlog to the subprocess spawn as its stderr, so it
    must be a real OS file descriptor."""
    async with ServerStderrRelay("plane") as relay:
        fd = relay.errlog.fileno()
        assert fd >= 0
        # Bytes written to the raw fd (as the child would) arrive too,
        # including invalid UTF-8 — decoded with errors="replace", never
        # crashing the reader.
        os.write(fd, b"raw bytes \xff\xfe here\n")
    assert any("raw bytes" in line for line in relay.tail())


@pytest.mark.asyncio
async def test_errlog_before_enter_raises():
    relay = ServerStderrRelay("plane")
    with pytest.raises(RuntimeError, match="not entered"):
        _ = relay.errlog


@pytest.mark.asyncio
async def test_reenter_after_exit_gives_fresh_pipe():
    """A relay is per-start(); exiting closes the pipe and a new context
    gets a new one (start after stop reuses nothing)."""
    relay = ServerStderrRelay("plane")
    async with relay:
        relay.errlog.write("first-run\n")
        relay.errlog.flush()
    async with relay:
        relay.errlog.write("second-run\n")
        relay.errlog.flush()
    # Both runs captured; the tail spans them (same relay object).
    assert "first-run" in relay.tail()
    assert "second-run" in relay.tail()
