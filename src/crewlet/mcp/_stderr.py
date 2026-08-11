"""Structured capture of a stdio MCP server's stderr.

The MCP SDK hands ``errlog`` to the subprocess spawn as the child's raw
stderr, so it must be a real OS pipe — anything the server (or the
package runner in front of it, e.g. ``uvx``/``npx``) prints would
otherwise splat unattributed into the engine's own console, interleaving
foreign log formats with Crewlet's structured stream.  With a dozen
per-role servers that is unreadable, and servers on older MCP SDKs also
dump a multi-line validation warning when they meet the modern
``server/discover`` handshake probe.

:class:`ServerStderrRelay` owns that pipe: a reader thread turns every
line into a ``server_stderr`` DEBUG event attributed to the server, and
keeps a bounded tail so a server that fails to start can have its last
words surfaced at ERROR by the wrapper (``server_stderr_tail``).
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import threading
from collections import deque
from typing import TextIO

from crewlet._logging import get_logger

logger = get_logger("mcp.stderr")

#: Crash-tail bound: enough for a Python traceback plus a server's startup
#: banner (a few KB per server), small enough to hold per spawned server.
_TAIL_LINES = 50

#: EOF on the pipe is immediate once the child (and our write end) close;
#: this bounds the wait for a leaked descriptor — a grandchild of the
#: server process (e.g. spawned by ``uvx``) that inherited stderr and
#: outlives the SDK's process stop.  The reader is a daemon thread, so an
#: expired wait leaks nothing that outlives the engine.
_JOIN_TIMEOUT_S = 5.0


class ServerStderrRelay:
    """Async context manager absorbing one server's stderr into logging.

    Enter it *before* the SDK client on the exit stack: LIFO teardown
    then closes the relay after the subprocess is gone, so the reader
    thread sees EOF and drains everything the server wrote.

    Usage::

        relay = await stack.enter_async_context(ServerStderrRelay(name))
        stdio_client(params, errlog=relay.errlog)
        ...
        relay.tail()   # last lines, for start-failure diagnostics
    """

    def __init__(self, server_name: str) -> None:
        self._server_name = server_name
        self._tail: deque[str] = deque(maxlen=_TAIL_LINES)
        self._errlog: TextIO | None = None
        self._thread: threading.Thread | None = None

    @property
    def errlog(self) -> TextIO:
        """The write end handed to ``stdio_client`` — a real OS pipe."""
        if self._errlog is None:
            msg = "ServerStderrRelay not entered"
            raise RuntimeError(msg)
        return self._errlog

    async def __aenter__(self) -> ServerStderrRelay:
        read_fd, write_fd = os.pipe()
        self._errlog = os.fdopen(write_fd, "w", encoding="utf-8", errors="replace")
        reader = os.fdopen(read_fd, "r", encoding="utf-8", errors="replace")
        self._thread = threading.Thread(
            target=self._pump,
            args=(reader,),
            name=f"mcp-stderr-{self._server_name}",
            daemon=True,
        )
        self._thread.start()
        return self

    def _pump(self, reader: TextIO) -> None:
        # Blocking readline loop; ends at EOF (child dead + write end
        # closed).  structlog's stdlib backend is thread-safe.
        with reader:
            for raw in reader:
                line = raw.rstrip("\r\n")
                if not line:
                    continue
                self._tail.append(line)
                logger.debug("server_stderr", server=self._server_name, line=line)

    async def __aexit__(self, *exc_info: object) -> None:
        if self._errlog is not None:
            with contextlib.suppress(OSError):
                self._errlog.close()
            self._errlog = None
        if self._thread is not None:
            await asyncio.to_thread(self._thread.join, _JOIN_TIMEOUT_S)
            if self._thread.is_alive():
                logger.debug("server_stderr_reader_lingering", server=self._server_name)
            self._thread = None

    def tail(self) -> list[str]:
        """The last lines the server wrote (bounded), oldest first."""
        return list(self._tail)
