"""Forge Invocation Token (FIT) verification.

Forge Remote sends a JWT (the FIT) in the ``Authorization: Bearer``
header with every request to the remote backend.  The token is
verified against Atlassian's public JWKS endpoint.

Verification checks:
1. Signature — via JWKS at ``FORGE_JWKS_URL``
2. Audience (``aud``) — must match the Forge app ID
3. Issuer (``iss``) — must be ``forge/invocation-token``
4. Expiration (``exp``) — must not be expired

Requires ``PyJWT[crypto]`` (``pip install pyjwt[crypto]``) for
RS256 verification with JWKS.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger

logger = get_logger("api.forge_fit")

FORGE_JWKS_URL = "https://forge.cdn.prod.atlassian-dev.net/.well-known/jwks.json"

_jwk_client: Any = None


async def verify_fit(
    token: str,
    app_id: str,
) -> dict[str, Any] | None:
    """Verify a Forge Invocation Token (FIT).

    Args:
        token: The JWT string from the ``Authorization: Bearer``
            header.
        app_id: The Forge app ID (``aud`` claim must match).

    Returns:
        The decoded token payload on success, or ``None`` on failure.

    Raises:
        RuntimeError: If ``PyJWT[crypto]`` is not installed.
    """
    try:
        import jwt  # PyJWT
        from jwt import PyJWKClient
    except ImportError as exc:
        logger.error(
            "pyjwt_crypto_not_installed",
            hint="Install PyJWT with crypto support: pip install 'pyjwt[crypto]'",
        )
        raise RuntimeError(
            "PyJWT with crypto support is required for Forge FIT verification"
        ) from exc

    try:
        global _jwk_client
        if _jwk_client is None:
            _jwk_client = PyJWKClient(FORGE_JWKS_URL)

        # PyJWKClient.get_signing_key_from_jwt() does synchronous
        # HTTP — run in a thread to avoid blocking the event loop.
        import anyio

        signing_key = await anyio.to_thread.run_sync(
            _jwk_client.get_signing_key_from_jwt, token
        )

        decoded = jwt.decode(
            token,
            signing_key.key,
            algorithms=["RS256"],
            audience=app_id,
            issuer="forge/invocation-token",
            options={"require": ["aud", "iss", "exp"]},
        )
        return decoded
    except jwt.ExpiredSignatureError:
        logger.warning("forge_fit_expired")
        return None
    except jwt.InvalidAudienceError:
        logger.warning("forge_fit_wrong_audience", expected=app_id)
        return None
    except jwt.InvalidIssuerError:
        logger.warning("forge_fit_wrong_issuer")
        return None
    except jwt.InvalidTokenError as exc:
        logger.warning("forge_fit_invalid", error=str(exc))
        return None
    except Exception as exc:
        logger.warning("forge_fit_verification_failed", error=str(exc))
        return None
