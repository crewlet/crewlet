"""Whole-config encryption + the read/write helpers every ``company_config``
payload consumer funnels through.

When a Tier A keyring is configured, the **entire** payload is encrypted
at rest as one opaque blob, stored as the wrapper
``{"__encrypted__": "enc:v1:<key>:<base64(json)>"}``. Nothing about the
config's structure — org chart, policies, model choices, or secrets — is
visible in the database. With no keyring, the payload is stored as-is
(unsealed plaintext / ``${VAR}``).

Consumers never handle the raw stored shape directly; they use:

- :func:`store_config` — encrypt a plaintext payload for persistence.
- :func:`load_config` — decrypt a stored payload back to plaintext (for
  construction: the engine, migrations, the CLI).
- :func:`redact_config` — a display-safe view: the structure is decrypted
  but every secret value is masked (for ``GET`` / dashboard / diff /
  export ``--redact``).
- :func:`rekey_config` — re-encrypt under the active key (rotation).

See ``docs/concepts/configuration.md`` § Secrets.
"""

from __future__ import annotations

import json
from typing import Any

from crewlet.secrets.cipher import (
    SecretCipher,
    SecretCipherError,
    is_envelope,
    parse_key_id,
)
from crewlet.secrets.registry import redact_payload

#: Sole key of the encrypted-document wrapper stored in the JSONB payload.
DOCUMENT_WRAPPER_KEY = "__encrypted__"

#: Associated data bound into the whole-document envelope. The blob is one
#: unit, so there is no per-field path to bind — a fixed label suffices and
#: still distinguishes a config blob from any other AAD domain.
DOCUMENT_AAD = "company_config/document"


def is_encrypted_document(stored: Any) -> bool:
    """True if ``stored`` is an encrypted-document wrapper."""
    return (
        isinstance(stored, dict)
        and set(stored) == {DOCUMENT_WRAPPER_KEY}
        and is_envelope(stored.get(DOCUMENT_WRAPPER_KEY))
    )


def encrypt_document(plaintext: dict[str, Any], cipher: SecretCipher) -> dict[str, Any]:
    """Encrypt a whole payload into the ``{"__encrypted__": …}`` wrapper.

    ``sort_keys`` makes the JSON deterministic so an unchanged config
    re-encrypts to the same plaintext (only the nonce differs).
    """
    blob = cipher.encrypt(
        json.dumps(plaintext, sort_keys=True, ensure_ascii=False),
        aad=DOCUMENT_AAD,
    )
    return {DOCUMENT_WRAPPER_KEY: blob}


def decrypt_document(stored: dict[str, Any], cipher: SecretCipher) -> dict[str, Any]:
    """Reverse :func:`encrypt_document` → the plaintext payload dict."""
    return json.loads(cipher.decrypt(stored[DOCUMENT_WRAPPER_KEY], aad=DOCUMENT_AAD))


def store_config(
    plaintext: dict[str, Any], cipher: SecretCipher | None
) -> dict[str, Any]:
    """Encrypt a payload for persistence.

    With a ``cipher`` the whole document is encrypted (``${VAR}`` inside
    is kept verbatim and resolved at construction). With ``cipher is
    None`` (encryption disabled) the payload is stored as-is.
    """
    if cipher is None:
        return dict(plaintext)
    return encrypt_document(plaintext, cipher)


def load_config(stored: Any, cipher: SecretCipher | None) -> dict[str, Any]:
    """Return the plaintext payload from a stored one.

    Decrypts an encrypted-document wrapper; passes a plaintext payload
    through unchanged. Fails closed if the payload is encrypted but no
    keyring is configured.
    """
    if is_encrypted_document(stored):
        if cipher is None:
            raise SecretCipherError(
                "config is stored encrypted but no encryption keyring is "
                "configured — set Tier A `secrets.keys`"
            )
        return decrypt_document(stored, cipher)
    return dict(stored)


def redact_config(stored: Any, cipher: SecretCipher | None) -> dict[str, Any]:
    """Display-safe payload: structure visible, every secret masked.

    Decrypts the document (the key is required to show any structure),
    then masks every secret leaf of the plaintext. When the blob can't be
    decrypted (no keyring), returns an opaque
    ``{"__encrypted__": {"encrypted": true}}`` rather than leaking. A
    plaintext (un-encrypted) payload is redacted directly.
    """
    if is_encrypted_document(stored):
        if cipher is None:
            return {DOCUMENT_WRAPPER_KEY: {"encrypted": True, "key_id": None}}
        return redact_payload(decrypt_document(stored, cipher))
    return redact_payload(stored)


def rekey_config(
    stored: Any, cipher: SecretCipher, active_key_id: str
) -> tuple[dict[str, Any], list[str]]:
    """Re-encrypt the document under ``active_key_id`` (rotation).

    A no-op when the document is already under the active key. Returns
    ``(new_payload, changed)`` where ``changed`` is ``[__encrypted__]``
    when the blob was rotated, else ``[]``.
    """
    if not is_encrypted_document(stored):
        return dict(stored), []
    if parse_key_id(stored[DOCUMENT_WRAPPER_KEY]) == active_key_id:
        return dict(stored), []
    plaintext = decrypt_document(stored, cipher)
    return encrypt_document(plaintext, cipher), [DOCUMENT_WRAPPER_KEY]


__all__ = [
    "DOCUMENT_AAD",
    "DOCUMENT_WRAPPER_KEY",
    "decrypt_document",
    "encrypt_document",
    "is_encrypted_document",
    "load_config",
    "redact_config",
    "rekey_config",
    "store_config",
]
