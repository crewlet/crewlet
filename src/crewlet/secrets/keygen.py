"""Generate fresh AES-256 keyring material for the Tier A ``secrets`` block.

``crewlet secrets keygen`` (wired in a later phase) prints a base64 key
plus the ``config.yaml`` / env-var snippet to install it. Key generation
is always an explicit operator action — there is no implicit "generate on
first boot", because a silently-generated key that isn't captured would
make every backup un-restorable.
"""

from __future__ import annotations

import base64
import os

from crewlet.env_file import format_assignment
from crewlet.secrets.cipher import KEY_BYTES


def generate_key() -> str:
    """Return a fresh base64-encoded 32-byte key suitable for the keyring."""
    return base64.b64encode(os.urandom(KEY_BYTES)).decode("ascii")


def keygen_snippet(key_id: str, *, env_var: str | None = None) -> str:
    """Return a copy-pasteable Tier A snippet wiring a new key in.

    ``env_var`` (default derived from ``key_id``) names the environment
    variable the material is read from, keeping the raw key out of
    ``config.yaml`` itself.
    """
    var = (
        env_var
        or f"CREWLET_SECRET_KEY_{key_id.upper().replace('-', '_').replace('.', '_')}"
    )
    key = generate_key()
    return (
        f"# 1. Export the key material (keep it out of config.yaml):\n"
        f"#    {format_assignment(var, key, export=True)}\n"
        f"# 2. Reference it from Tier A config.yaml:\n"
        f"secrets:\n"
        f"  active_key_id: {key_id}\n"
        f"  keys:\n"
        f"    - id: {key_id}\n"
        f'      material: "${{{var}}}"\n'
    )


__all__ = ["generate_key", "keygen_snippet"]
