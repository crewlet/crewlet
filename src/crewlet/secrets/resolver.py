"""Process-local secret source consulted ahead of ``os.environ``.

``${VAR}`` resolution has exactly one choke point —
``crewlet.config._resolve_env_value`` — and its only source used to be
``os.getenv``.  That is why provisioners write ``.env`` files: the
environment was the only address the engine could read back.

This module adds a second source.  A :class:`SecretSource` is installed
once per process (at boot, from the encrypted ``secret_values`` table);
the resolver asks it first and falls back to the environment.  Nothing
else changes: substitution stays synchronous, the reference syntax stays
the same, and a process with no source installed behaves exactly as
before.

**Store wins, environment is the fallback.**  The reverse order would
re-introduce the failure this exists to fix — a stale ``.env`` shadowing
a freshly rotated secret, surfacing days later as an auth error from the
provider rather than at the point of rotation.  When a name exists in
both with different values, :func:`install_secret_source` logs
``secret_shadowed_env`` at WARNING with the **names only**.

The module holds a process global on purpose.  Resolution happens at ~25
scattered use sites (per-role MCP spawn, per-search knowledge tokens, per
sandbox launch), all synchronous and none of them plumbed for an extra
argument; threading a source parameter through every one of them would be
a far larger change with no benefit over installing it once.

This module imports nothing from :mod:`crewlet.config` — the dependency
runs the other way.

See ``docs/concepts/secret-store.md``.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from crewlet._logging import get_logger

logger = get_logger("secrets.resolver")


@runtime_checkable
class SecretSource(Protocol):
    """A synchronous lookup of secret values by environment-variable name."""

    def get(self, name: str) -> str | None:
        """The stored value for ``name``, or ``None`` when not stored.

        ``None`` means "this source has nothing to say" — the resolver
        then falls back to ``os.environ``.  A stored **empty** value is
        returned as ``""`` and is authoritative, never a miss.
        """
        ...


#: The installed source, or ``None`` when the feature is inert. Process
#: global by design — see the module docstring.
_SOURCE: SecretSource | None = None


def install_secret_source(source: SecretSource | None) -> None:
    """Install (or with ``None``, remove) the process-wide secret source.

    Logs a one-line inventory of what the source shadows, so an operator
    can see at boot which names now come from the database instead of the
    environment.  Names only — a value never reaches the log.
    """
    global _SOURCE  # noqa: PLW0603
    _SOURCE = source
    if source is None:
        logger.debug("secret_source_cleared")
        return
    _log_shadowed(source)


def _log_shadowed(source: SecretSource) -> None:
    """Warn about names the store and the environment both define.

    Only meaningful for a source that can enumerate itself (the database
    snapshot does); an opaque source is installed silently.
    """
    import os

    names = getattr(source, "names", None)
    if not callable(names):
        return
    shadowed = sorted(
        name
        for name in names()
        if name in os.environ and os.environ[name] != source.get(name)
    )
    if shadowed:
        logger.warning(
            "secret_shadowed_env",
            names=shadowed,
            detail=(
                "the secret store and the environment disagree on these "
                "names; the stored value wins. Remove the stale export or "
                ".env entry to silence this."
            ),
        )


def active_secret_source() -> SecretSource | None:
    """The installed source, or ``None``."""
    return _SOURCE


def resolve_env(name: str, default: str = "") -> str:
    """``os.getenv`` with the secret store consulted first.

    For the handful of call sites that read a variable by NAME rather than
    resolving a ``${VAR}`` reference — the providers' conventional-key
    fallbacks (``OPENAI_API_KEY``, ``ANTHROPIC_API_KEY``). Without this
    they would answer "unset" for a credential the operator deliberately
    stored, so ``crewlet secrets set OPENAI_API_KEY`` would work through a
    config reference but not through the fallback, which is precisely the
    kind of inconsistency that turns into a support thread.

    Deliberately NOT used for deployment-environment settings (the DSN,
    OTLP endpoints, operator provisioning credentials) — those are read
    before the store exists, or belong to the host rather than the
    company.
    """
    import os

    stored = lookup_secret(name)
    if stored is not None:
        return stored
    return os.getenv(name, default)


async def refresh_secret_snapshot() -> None:
    """Re-read the installed source, when it supports refreshing.

    A no-op with no source installed (or an opaque one), so callers on the
    config-apply path need no conditional. Refresh failures propagate:
    continuing with a snapshot that silently went stale is the failure
    mode the store exists to remove.
    """
    source = _SOURCE
    if source is None:
        return
    refresh = getattr(source, "refresh", None)
    if not callable(refresh):
        return
    await refresh()
    _log_shadowed(source)


def lookup_secret(name: str) -> str | None:
    """The stored value for ``name``, or ``None`` when there is none.

    ``None`` covers both "no source installed" and "this source does not
    carry that name", which is what makes the caller's ``os.getenv``
    fallback the single fallback path.
    """
    source = _SOURCE
    if source is None:
        return None
    return source.get(name)


__all__ = [
    "SecretSource",
    "active_secret_source",
    "install_secret_source",
    "lookup_secret",
    "refresh_secret_snapshot",
]
