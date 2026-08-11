"""Central structured logging configuration for Crewlet.

Every module should obtain its logger via :func:`get_logger` so that a
``component`` key is automatically bound to every log line.
"""

from __future__ import annotations

import logging
import sys
from typing import Any

import structlog

_configured = False

_SHARED_PROCESSORS: list[Any] = [
    structlog.contextvars.merge_contextvars,
    structlog.stdlib.add_logger_name,
    structlog.stdlib.add_log_level,
    structlog.processors.TimeStamper(fmt="iso"),
    structlog.processors.StackInfoRenderer(),
    structlog.processors.format_exc_info,
]


def _configure_structlog() -> None:
    """Route structlog through the stdlib logging system.

    Called **at import** so every module-level ``get_logger(...)`` (created
    before any ``configure_logging`` call) binds to the stdlib
    ``LoggerFactory`` — NOT structlog's default ``PrintLogger``, which
    writes to **stdout** and would leak log lines into command output
    (e.g. ``crewlet config export``). With the stdlib factory, output is
    governed entirely by the root handler that :func:`configure_logging`
    installs on **stderr**; until then, stdlib's own default suppresses
    sub-warning records, so nothing reaches stdout.
    """
    structlog.configure(
        processors=[
            *_SHARED_PROCESSORS,
            structlog.stdlib.ProcessorFormatter.wrap_for_formatter,
        ],
        logger_factory=structlog.stdlib.LoggerFactory(),
        wrapper_class=structlog.stdlib.BoundLogger,
        cache_logger_on_first_use=True,
    )


_configure_structlog()


def configure_logging(
    level: int = logging.INFO,
    fmt: str | None = None,
) -> None:
    """Install the root log handler (stderr) at ``level``.  Call once at startup.

    Args:
        level: logging level (e.g. ``logging.DEBUG``).
        fmt: ``"console"`` for coloured human output, ``"json"`` for
            machine-readable JSON lines, or ``None`` to auto-detect
            from whether *stderr* is a TTY.
    """
    global _configured  # noqa: PLW0603
    if _configured:
        return
    _configured = True

    if fmt is None:
        fmt = "console" if sys.stderr.isatty() else "json"

    if fmt == "json":
        renderer: Any = structlog.processors.JSONRenderer()
    else:
        renderer = structlog.dev.ConsoleRenderer()

    formatter = structlog.stdlib.ProcessorFormatter(
        processors=[
            structlog.stdlib.ProcessorFormatter.remove_processors_meta,
            renderer,
        ],
    )

    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(formatter)

    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(level)


def get_logger(component: str) -> Any:
    """Return a structlog logger pre-bound with ``component=<component>``."""
    name = f"crewlet.{component}"
    return structlog.get_logger(name).bind(component=component)


def reset_logging() -> None:
    """Reset the configured state (for testing only)."""
    global _configured  # noqa: PLW0603
    _configured = False
