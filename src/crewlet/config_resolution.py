"""Resolution fingerprint — "did any resolved credential change?"

A config revision and the *values* its ``${VAR}`` references resolve to are
two different things, and the engine only ever compared the first.

That gap is the whole of secret rotation. The documented gesture for
picking up a rotated credential is to **re-activate the unchanged
revision** — which means the payload is byte-identical, which means
``apply_config``'s no-op early-out fires and nothing rebuilds. The secret
snapshot is swapped and every subsystem that captured a resolved value
keeps the old one: MCP subprocesses baked it into their spawn env, LLM
providers hold it in a client, transports hold it in a header.

The fingerprint closes that: a digest over the *resolved* value of every
variable the payload references. Equal payload **and** equal fingerprint
is a true no-op; equal payload with a changed fingerprint is a rotation
and has to rebuild.

**The digest is keyed, per process, and never leaves it.** A bare hash of
a short credential in a log line or a database row is offline-brute-
forceable, which would turn a fix into a leak. The key is random at
import, so a fingerprint is meaningful only as "has this changed since
the last apply *in this process*" — which is exactly, and only, what it
is asked.
"""

from __future__ import annotations

import hashlib
import secrets
from typing import Any

from crewlet.env_refs import ENV_VAR_PATTERN

# Random per process, never persisted. See the module docstring: this
# makes a fingerprint incomparable across processes and useless to an
# attacker who obtains one, at no cost to its only use.
_FINGERPRINT_KEY = secrets.token_bytes(32)

# Note on unset variables: the config layer resolves an unset ``${VAR}``
# to the empty string, and every builder consumes it that way. The
# fingerprint deliberately inherits that rather than distinguishing the
# two — a difference the engine cannot act on is not worth reporting, and
# introducing one here would make the fingerprint disagree with what the
# builders actually see.


def _walk_strings(data: Any) -> list[str]:
    """Every string in a nested payload, in a stable order."""
    out: list[str] = []
    if isinstance(data, dict):
        for key in sorted(data, key=str):
            out.extend(_walk_strings(data[key]))
    elif isinstance(data, (list, tuple)):
        for item in data:
            out.extend(_walk_strings(item))
    elif isinstance(data, str):
        out.append(data)
    return out


def referenced_names(payload: Any) -> list[str]:
    """Sorted, de-duplicated ``${VAR}`` names referenced anywhere in ``payload``."""
    names: set[str] = set()
    for text in _walk_strings(payload):
        for match in ENV_VAR_PATTERN.finditer(text):
            names.add(match.group(1))
    return sorted(names)


def resolution_fingerprint(payload: Any) -> str:
    """Keyed digest over what this payload's ``${VAR}`` references resolve to.

    Resolution goes through the same path the config layer uses, so the
    secret store is consulted ahead of the environment exactly as it is
    for a real build — otherwise a rotation written to the store would
    not register.

    Returns a short hex string. Two calls in one process are comparable;
    a value from another process is not, and is never meant to be.
    """
    from crewlet.config import _resolve_env_value

    digest = hashlib.blake2b(key=_FINGERPRINT_KEY, digest_size=16)
    for name in referenced_names(payload):
        value = _resolve_env_value(f"${{{name}}}")
        # Length-prefixed so ("ab", "c") and ("a", "bc") cannot collide.
        digest.update(f"{len(name)}:{name}={len(str(value))}:{value}\n".encode())
    return digest.hexdigest()
