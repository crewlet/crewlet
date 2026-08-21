"""THE secret-redaction pass for free text leaving the engine.

Anything the engine ships to a place a person or a model reads — a tool
result, a coding-agent transcript, a setup-step failure, an event-store
payload — can carry a credential it never meant to. A cloned repo URL
with the token in it, a CLI that echoes its key on failure, a
provisioning command whose ``${VAR}`` was resolved before it ran.

This module is that pass, and there is deliberately only one of it: a
second copy is how a surface ends up redacting a token shape its
neighbour already knows about. It lives here, importing nothing from
Crewlet, so every layer can depend on it — it started inside the turn
engine's tool loop, which meant the sandbox layer had to import the
agent layer to redact a string, and one more such import closed a cycle
through ``crewlet/__init__``.

The patterns are a denylist of known credential SHAPES, which bounds
what this can promise: it catches the vendor prefixes and key formats
below, not an arbitrary opaque secret. It is the last line, not the
first — the first is not putting a credential in the text.
"""

from __future__ import annotations

import re

#: Patterns that indicate secrets / credentials in free text. One list,
#: so every surface redacts the same shapes.
_SECRET_PATTERNS: list[tuple[re.Pattern[str], str]] = [
    (re.compile(r"sk-proj-[A-Za-z0-9_-]{20,}"), "[REDACTED:api-key]"),
    (re.compile(r"sk-[A-Za-z0-9_-]{20,}"), "[REDACTED:api-key]"),
    (re.compile(r"xoxb-[A-Za-z0-9-]{20,}"), "[REDACTED:slack-token]"),
    (re.compile(r"xoxp-[A-Za-z0-9-]{20,}"), "[REDACTED:slack-token]"),
    (re.compile(r"xoxs-[A-Za-z0-9-]{20,}"), "[REDACTED:slack-token]"),
    (re.compile(r"AKIA[A-Z0-9]{16}"), "[REDACTED:aws-key]"),
    (re.compile(r"ghp_[A-Za-z0-9]{36,}"), "[REDACTED:github-token]"),
    (re.compile(r"gho_[A-Za-z0-9]{36,}"), "[REDACTED:github-token]"),
    (re.compile(r"glpat-[A-Za-z0-9_-]{20,}"), "[REDACTED:gitlab-token]"),
    (re.compile(r"plane_api_[A-Za-z0-9_-]{20,}"), "[REDACTED:plane-token]"),
    (re.compile(r"plane_wh_[A-Za-z0-9_-]{20,}"), "[REDACTED:plane-webhook-secret]"),
    (
        re.compile(
            r"-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"
            r"[\s\S]*?"
            r"-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"
        ),
        "[REDACTED:private-key]",
    ),
    (re.compile(r"(?i)(?:password|passwd|pwd)\s*[:=]\s*\S+"), "[REDACTED:password]"),
]


def redact_secrets(text: str) -> str:
    """Replace known secret / credential patterns with markers.

    Idempotent: a marker matches nothing here, so redacting twice is
    the same as redacting once.
    """
    for pattern, replacement in _SECRET_PATTERNS:
        text = pattern.sub(replacement, text)
    return text


__all__ = ["redact_secrets"]
