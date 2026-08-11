"""Authenticated-encryption cipher for company-config secrets at rest.

A :class:`SecretCipher` seals a plaintext secret into a self-describing
``enc:v1:<key_id>:<base64>`` envelope and reverses the transform. The
concrete :class:`KeyringCipher` uses AES-256-GCM (AEAD) with a per-value
random nonce; the field's JSON-pointer path is bound in as *associated
data* so a ciphertext cannot be replayed into a different field.

The envelope carries the ``key_id`` that sealed it, so one revision can
hold ciphertexts under a mix of keys during a key rotation and each
decrypts under the right entry of the keyring.

See ``docs/concepts/configuration.md`` § Secrets.
"""

from __future__ import annotations

import base64
import binascii
import os
from typing import TYPE_CHECKING, Protocol, runtime_checkable

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

if TYPE_CHECKING:
    from crewlet.config import SecretsConfig

#: Prefix + format-version tag every envelope carries. Bumping the ``v1``
#: is how a future algorithm change stays distinguishable from existing
#: ciphertext.
ENVELOPE_PREFIX = "enc:v1:"

#: AES-256 key size in bytes.
KEY_BYTES = 32

#: GCM nonce size in bytes (96 bits — the AES-GCM standard nonce length).
NONCE_BYTES = 12


class SecretCipherError(Exception):
    """Base error for secret sealing / unsealing."""


class SecretDecryptError(SecretCipherError):
    """Raised when an envelope cannot be decrypted.

    Covers every failure mode uniformly — wrong key, unknown ``key_id``,
    tampered ciphertext, or an associated-data (field-path) mismatch —
    so a caller cannot distinguish them and use the difference as an
    oracle.
    """


@runtime_checkable
class SecretCipher(Protocol):
    """Seals and unseals a single secret string.

    Implementations must be deterministic on ``decrypt`` and bind
    ``aad`` (the field's JSON-pointer path) so a ciphertext moved to a
    different field fails to decrypt.
    """

    def encrypt(self, plaintext: str, *, aad: str) -> str:
        """Return an ``enc:v1:<key_id>:<base64>`` envelope for ``plaintext``."""
        ...

    def decrypt(self, token: str, *, aad: str) -> str:
        """Reverse :meth:`encrypt`. Raise :class:`SecretDecryptError` on failure."""
        ...


def is_envelope(value: object) -> bool:
    """True when ``value`` is a well-formed ``enc:v1:<key_id>:<blob>`` token."""
    if not isinstance(value, str) or not value.startswith(ENVELOPE_PREFIX):
        return False
    # "enc", "v1", key_id, blob — four colon-separated parts, key_id and
    # blob both non-empty.
    parts = value.split(":", 3)
    return len(parts) == 4 and bool(parts[2]) and bool(parts[3])


def parse_key_id(token: str) -> str | None:
    """Return the ``key_id`` an envelope names, or ``None`` if not an envelope."""
    if not is_envelope(token):
        return None
    return token.split(":", 3)[2]


def decode_key_material(material: str) -> bytes:
    """Decode a base64 key string into raw 32 bytes, validating length.

    ``material`` is the resolved value of a Tier A ``secrets.keys[].material``
    entry (``${VAR}`` already substituted at bootstrap load time).
    """
    try:
        raw = base64.b64decode(material, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise SecretCipherError("secret key material must be valid base64") from exc
    if len(raw) != KEY_BYTES:
        raise SecretCipherError(
            f"secret key material must decode to {KEY_BYTES} bytes "
            f"(got {len(raw)}); generate one with `crewlet secrets keygen`"
        )
    return raw


class KeyringCipher:
    """AES-256-GCM cipher over a small keyring.

    ``keys`` maps ``key_id`` → raw 32-byte key; ``active_key_id`` names
    the entry used to seal new writes. Every configured key stays
    available for :meth:`decrypt`, so a rotation (new active key, old key
    retained) decrypts mixed-key revisions without downtime.
    """

    def __init__(self, keys: dict[str, bytes], active_key_id: str) -> None:
        if not keys:
            raise SecretCipherError("keyring is empty")
        if active_key_id not in keys:
            raise SecretCipherError(
                f"active_key_id {active_key_id!r} is not in the keyring"
            )
        for key_id, material in keys.items():
            if len(material) != KEY_BYTES:
                raise SecretCipherError(
                    f"key {key_id!r} is {len(material)} bytes, expected {KEY_BYTES}"
                )
        self._keys = dict(keys)
        self._active_key_id = active_key_id

    @classmethod
    def from_config(cls, config: SecretsConfig) -> KeyringCipher | None:
        """Build a cipher from Tier A ``secrets`` config, or ``None`` if unset.

        Returns ``None`` when no keys are configured — the caller treats
        that as "secret encryption disabled" and fails closed only if the
        active revision actually contains sealed values.
        """
        if not config.keys:
            return None
        keys = {k.id: decode_key_material(k.material) for k in config.keys}
        return cls(keys=keys, active_key_id=config.active_key_id)

    @property
    def active_key_id(self) -> str:
        return self._active_key_id

    def encrypt(self, plaintext: str, *, aad: str) -> str:
        key = self._keys[self._active_key_id]
        nonce = os.urandom(NONCE_BYTES)
        ct = AESGCM(key).encrypt(nonce, plaintext.encode("utf-8"), aad.encode("utf-8"))
        blob = base64.b64encode(nonce + ct).decode("ascii")
        return f"{ENVELOPE_PREFIX}{self._active_key_id}:{blob}"

    def decrypt(self, token: str, *, aad: str) -> str:
        if not is_envelope(token):
            raise SecretDecryptError("value is not an enc:v1 envelope")
        _, _, key_id, blob = token.split(":", 3)
        key = self._keys.get(key_id)
        if key is None:
            raise SecretDecryptError(
                f"no key for key_id {key_id!r} in the keyring — the sealing "
                f"key is missing from Tier A `secrets.keys`"
            )
        try:
            raw = base64.b64decode(blob, validate=True)
            nonce, ct = raw[:NONCE_BYTES], raw[NONCE_BYTES:]
            plaintext = AESGCM(key).decrypt(nonce, ct, aad.encode("utf-8"))
        except (InvalidTag, binascii.Error, ValueError) as exc:
            raise SecretDecryptError(
                "decryption failed (wrong key, tampered ciphertext, or "
                "field-path mismatch)"
            ) from exc
        return plaintext.decode("utf-8")


__all__ = [
    "ENVELOPE_PREFIX",
    "KEY_BYTES",
    "NONCE_BYTES",
    "KeyringCipher",
    "SecretCipher",
    "SecretCipherError",
    "SecretDecryptError",
    "decode_key_material",
    "is_envelope",
    "parse_key_id",
]
