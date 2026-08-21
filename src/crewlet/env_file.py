"""THE ``.env`` assignment grammar — one writer for two provisioners.

A provisioning run mints credentials that are unretrievable afterwards,
and an env file is where several of the CLIs put them.  What that file
must guarantee is round-tripping: the value the engine reads back, and
the value a shell gets from ``source``, have to be the value that was
minted.  Two very ordinary characters break that when a value is
written bare — a space makes ``source`` fail on the line (and, because
``source`` stops there, silently abandons every credential BELOW it),
and ``${…}`` is interpolated away by python-dotenv in the reader the
engine actually uses.

This module is that grammar, in one place, because it was in two: the
Slack provisioner reasoned it through and refused the shapes it could
not represent, while the shared ``TokenSink`` env-file backend wrote
``f"{var}={token}"`` and hoped.  Both write secrets, so a divergence
means the safety of a credential depends on which CLI minted it.

It imports nothing from Crewlet, like :mod:`crewlet.env_refs` and
:mod:`crewlet.queue.topics`, so every writer can depend on it —
and ``tests/test_provisioning/test_env_file.py`` fails the build on a
second definition of the grammar.
"""

from __future__ import annotations

import re

#: Matches ``KEY=…`` / ``export KEY=…`` assignment lines.
ASSIGNMENT_RE = re.compile(r"^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=")

#: Characters that force quoting.  ``$`` is included because python-dotenv
#: interpolates ``${VAR}`` inside unquoted and double-quoted values —
#: only single quotes disable it, so ``$`` routes to the single-quoted
#: form below.
_NEEDS_QUOTING_RE = re.compile(r"[\s#'\"\\$]")

_EXPORT_PREFIX = "export "


class EnvValueError(ValueError):
    """A value with no faithful ``.env`` representation."""


def format_assignment(key: str, value: str, *, export: bool = False) -> str:
    """Render one ``KEY=value`` line that reads back verbatim.

    Forms, chosen by content:

    - plain ``KEY=value`` when no character needs quoting;
    - ``KEY='value'`` when the value contains a bare ``$`` (quoting
      keeps it out of the double-quote escape rules);
    - ``KEY="value"`` with backslash escapes otherwise.

    ``export`` prefixes the line, which is the shape ``--print`` emits
    so pasted output round-trips; every quoting form above stays valid
    after it.

    Two value shapes have NO faithful python-dotenv representation and
    are refused loudly rather than persisted as something the engine
    would read back differently: ``${…}`` sequences (python-dotenv
    interpolates them in EVERY quoting form, single quotes included),
    and a bare ``$`` combined with a single quote (the only safe quoting
    for ``$`` can't contain ``'``).  Real credentials are URL-safe
    strings, so neither shape occurs in practice — but a refusal an
    operator can read beats a token that silently changes on its way
    back out.
    """
    prefix = _EXPORT_PREFIX if export else ""
    if value and not _NEEDS_QUOTING_RE.search(value):
        return f"{prefix}{key}={value}"
    if "${" in value:
        msg = (
            f"cannot faithfully store {key}: python-dotenv interpolates "
            "${...} sequences in every quoting form, so the value would "
            "read back changed"
        )
        raise EnvValueError(msg)
    if "$" in value:
        if "'" in value:
            msg = (
                f"cannot faithfully store {key}: the value combines '$' "
                "and a single quote, which python-dotenv cannot round-trip"
            )
            raise EnvValueError(msg)
        return f"{prefix}{key}='{value}'"
    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
    return f'{prefix}{key}="{escaped}"'


def parse_assignment(line: str) -> tuple[str, str] | None:
    """Split one assignment line into ``(key, value)``, or ``None``.

    The read half of :func:`format_assignment`, and deliberately its
    exact inverse: it unwraps either quoting form and undoes the
    double-quoted escapes.  A reader that only stripped the quotes
    would hand back ``a\\"b`` for a value that was written as ``a"b``.
    """
    match = ASSIGNMENT_RE.match(line)
    if match is None:
        return None
    key = match.group(1)
    raw = line.split("=", 1)[1].strip()
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "'\"":
        inner = raw[1:-1]
        if raw[0] == '"':
            inner = inner.replace('\\"', '"').replace("\\\\", "\\")
        return key, inner
    return key, raw


def is_exported(line: str) -> bool:
    """Whether an assignment line carries the ``export`` prefix."""
    return line.strip().startswith(_EXPORT_PREFIX)


__all__ = [
    "ASSIGNMENT_RE",
    "EnvValueError",
    "format_assignment",
    "is_exported",
    "parse_assignment",
]
