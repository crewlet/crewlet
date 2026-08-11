"""Company-config encryption at rest.

When a Tier A keyring is configured, the **entire** ``company_config``
payload is encrypted as one opaque blob at rest — org structure,
policies, model choices, and secrets are all ciphertext in the database.
Sealed on every write, decrypted for construction, and shown redacted
(structure visible, secrets masked) on every read.

The keyring that seals / unseals is sourced from Tier A
(``config.yaml`` ``secrets:`` block) and is the sole root of trust: the
database holds only ciphertext, never the key.

See ``docs/concepts/configuration.md`` § Secrets.
"""

from crewlet.secrets.cipher import (
    ENVELOPE_PREFIX,
    KeyringCipher,
    SecretCipher,
    SecretCipherError,
    SecretDecryptError,
    decode_key_material,
    is_envelope,
    parse_key_id,
)
from crewlet.secrets.document import (
    DOCUMENT_WRAPPER_KEY,
    decrypt_document,
    encrypt_document,
    is_encrypted_document,
    load_config,
    redact_config,
    rekey_config,
    store_config,
)
from crewlet.secrets.keygen import generate_key, keygen_snippet
from crewlet.secrets.registry import (
    SecretLeakError,
    is_redaction_marker,
    redact_payload,
    restore_redacted,
    secret_pointers,
)
from crewlet.secrets.resolver import (
    SecretSource,
    active_secret_source,
    install_secret_source,
    lookup_secret,
)

__all__ = [
    "DOCUMENT_WRAPPER_KEY",
    "ENVELOPE_PREFIX",
    "KeyringCipher",
    "SecretCipher",
    "SecretCipherError",
    "SecretDecryptError",
    "SecretLeakError",
    "SecretSource",
    "active_secret_source",
    "decode_key_material",
    "decrypt_document",
    "encrypt_document",
    "generate_key",
    "install_secret_source",
    "is_encrypted_document",
    "is_envelope",
    "is_redaction_marker",
    "keygen_snippet",
    "load_config",
    "lookup_secret",
    "parse_key_id",
    "redact_config",
    "redact_payload",
    "rekey_config",
    "restore_redacted",
    "secret_pointers",
    "store_config",
]
