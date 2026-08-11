"""In-process fake cipher for tests — reversible, deterministic, NOT secure.

Mirrors the ``sandbox/fake.py`` pattern: a stand-in that satisfies the
:class:`~crewlet.secrets.cipher.SecretCipher` protocol without real
crypto, so document encrypt / decrypt / redact tests are deterministic
and don't depend on random nonces. It still binds ``aad`` so tampering
with the associated data is caught, and produces the same
``enc:v1:<key_id>:<blob>`` envelope shape the real cipher does.
"""

from __future__ import annotations

import base64

from crewlet.secrets.cipher import ENVELOPE_PREFIX, SecretDecryptError, is_envelope

_FAKE_KEY_ID = "fake"
_SEP = "\x00"


class FakeCipher:
    """Reversible, deterministic, non-cryptographic cipher for tests."""

    def __init__(self, key_id: str = _FAKE_KEY_ID) -> None:
        self._key_id = key_id

    @property
    def active_key_id(self) -> str:
        return self._key_id

    def encrypt(self, plaintext: str, *, aad: str) -> str:
        blob = base64.b64encode(f"{aad}{_SEP}{plaintext}".encode()).decode("ascii")
        return f"{ENVELOPE_PREFIX}{self._key_id}:{blob}"

    def decrypt(self, token: str, *, aad: str) -> str:
        if not is_envelope(token):
            raise SecretDecryptError("value is not an enc:v1 envelope")
        _, _, _key_id, blob = token.split(":", 3)
        try:
            raw = base64.b64decode(blob, validate=True).decode("utf-8")
        except Exception as exc:  # noqa: BLE001 — uniform failure mode
            raise SecretDecryptError("fake decrypt failed") from exc
        stored_aad, sep, plaintext = raw.partition(_SEP)
        if sep != _SEP or stored_aad != aad:
            raise SecretDecryptError("field-path (aad) mismatch")
        return plaintext


__all__ = ["FakeCipher"]
