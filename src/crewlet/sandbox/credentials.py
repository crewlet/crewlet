"""Assemble the sandbox run env — TOOL-AGNOSTIC engine facts only.

The engine contributes exactly two kinds of env, neither specific to any
external tool: the LLM creds the role already uses (its resolved
``providers.llm`` entry — the engine chose the model, so it threads the
credential) and the agent's IDENTITY (``CREWLET_AGENT_HANDLE`` /
``CREWLET_AGENT_EMAIL`` — per-launch facts static config cannot know, which
setup-step recipes map into whatever their tool needs, e.g. the git-auth
recipe's ``git config user.name "$CREWLET_AGENT_HANDLE"``).

Everything tool-specific comes from config: external-service tokens
(``GITHUB_TOKEN`` and friends) are declared in ``role.sandbox.env`` /
setup-step ``env`` and merely ``${VAR}``-resolved here — the engine never
extracts, names, or special-cases them. The OTLP ingest token is **never**
injected; only non-secret OTel values are.
"""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.config import (
    LLMProviderConfig,
    env_reference_is_resolvable,
    resolve_env_vars,
)

logger = get_logger("sandbox.credentials")

# Anthropic-compatible signals that satisfy Claude Code's auth requirement.
_CLAUDE_AUTH_KEYS = (
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "CLAUDE_CODE_USE_BEDROCK",
    "CLAUDE_CODE_USE_VERTEX",
    "CLAUDE_CODE_USE_FOUNDRY",
)


class SandboxCredentialError(ValueError):
    """The role's config can't satisfy the chosen coding agent's creds."""


def _llm_env(coding_agent: str, llm_config: LLMProviderConfig) -> dict[str, str]:
    """Env vars that point the coding agent at the role's LLM provider.

    Claude Code only speaks the Anthropic API, so a non-Anthropic provider
    yields nothing here -- the caller's validation then requires an explicit
    key in ``role.sandbox.env``. OpenCode is provider-agnostic.
    """
    keys = llm_config.resolved_keys()
    api_key = keys[0] if keys else ""
    env: dict[str, str] = {}
    if coding_agent == "claude-code":
        if llm_config.type == "anthropic":
            if api_key:
                env["ANTHROPIC_API_KEY"] = api_key
            if llm_config.base_url:
                env["ANTHROPIC_BASE_URL"] = llm_config.base_url
        # non-anthropic → nothing; role.sandbox.env must supply the key
    else:  # opencode (provider-agnostic)
        anthropic = llm_config.type == "anthropic"
        if api_key:
            # The OpenCode custom provider references this via {env:...}
            # rather than inlining the secret (see opencode.to_opencode_provider).
            env["ANTHROPIC_API_KEY" if anthropic else "OPENAI_API_KEY"] = api_key
        # Also export the endpoint. NOTE: for OpenCode a *_BASE_URL env does
        # NOT redirect a built-in provider — the real redirect is the custom
        # provider the runner writes into opencode.json. This is
        # kept for parity / co-located collectors; it's harmless when the
        # custom provider's baseURL is what OpenCode actually uses.
        if llm_config.base_url:
            env["ANTHROPIC_BASE_URL" if anthropic else "OPENAI_BASE_URL"] = (
                llm_config.base_url
            )
    return env


def build_sandbox_env(
    *,
    coding_agent: str,
    llm_config: LLMProviderConfig,
    role_sandbox_env: dict[str, str] | None = None,
    agent_handle: str = "",
    agent_email: str = "",
    setup_env: dict[str, str] | None = None,
    otel_env: dict[str, str] | None = None,
) -> dict[str, str]:
    """Assemble + resolve the full env injected into the sandbox.

    The engine contributes only tool-agnostic facts: the LLM creds and the
    agent's identity as ``CREWLET_AGENT_HANDLE`` / ``CREWLET_AGENT_EMAIL``
    (per-launch values static config cannot know — setup-step recipes map
    them into tool-specific shape, e.g. git commit identity). Every
    tool-specific variable (``GITHUB_TOKEN``, registry tokens, …) comes
    from ``setup_env`` (the merged step contributions,
    :func:`crewlet.sandbox.setup.setup_env`) or ``role.sandbox.env``.

    Precedence (later wins): derived LLM creds → agent identity →
    setup-step env → ``role.sandbox.env`` overrides → non-secret OTel
    values. ``${ENV}`` references are resolved at the end (like
    ``mcp_env``); any reference whose variable is unset or empty — whether
    the whole value (``"${TOKEN}"``) or embedded (``"Bearer ${TOKEN}"``) —
    logs a ``sandbox_env_unresolved`` warning naming the keys (never the
    values), so a seat whose token env var isn't exported fails loudly
    instead of mysteriously.
    Raises :class:`SandboxCredentialError` when Claude Code is chosen but
    no Anthropic-compatible credential is reachable.
    """
    env: dict[str, str] = {}
    env.update(_llm_env(coding_agent, llm_config))
    if agent_handle:
        env["CREWLET_AGENT_HANDLE"] = agent_handle
    if agent_email:
        env["CREWLET_AGENT_EMAIL"] = agent_email
    env.update(setup_env or {})
    env.update(role_sandbox_env or {})
    if otel_env:
        env.update(otel_env)

    # Flag per REFERENCE, not per final value: an embedded form like
    # "Bearer ${TOKEN}" resolves to a truthy-but-broken "Bearer " when the
    # var is unset, so testing the resolved value would miss exactly the
    # composite shapes role.sandbox.env documents. Keys only — never values,
    # which may embed partial secrets. Resolvability is asked of the engine
    # (secret store, then environment), not of os.environ directly, so a
    # credential that lives only in the store does not read as missing.
    unresolved = sorted(
        k
        for k, v in env.items()
        if isinstance(v, str) and not env_reference_is_resolvable(v)
    )
    if unresolved:
        logger.warning("sandbox_env_unresolved", keys=unresolved)

    resolved = resolve_env_vars(env)

    # Validate AFTER resolution. The pre-resolution dict holds credential
    # values verbatim (``resolved_keys()`` returns "${ANTHROPIC_KEY_1}" as
    # written), and a raw reference is truthy — so a key check against it
    # passed for an UNSET variable and Claude Code launched with an empty
    # credential instead of raising here. Requiring a non-empty resolved
    # value is what makes this function's promise ("raises when no
    # Anthropic-compatible credential is reachable") actually true.
    if coding_agent == "claude-code" and not any(
        resolved.get(k) for k in _CLAUDE_AUTH_KEYS
    ):
        raise SandboxCredentialError(
            "coding_agent 'claude-code' needs an Anthropic-compatible "
            f"provider (the resolved sandbox provider is type "
            f"{llm_config.type!r}). Point role.llm_sandbox at an 'anthropic' "
            "providers.llm entry, or set ANTHROPIC_API_KEY in role.sandbox.env. "
            "A ${VAR} reference that resolves to nothing counts as missing."
        )
    return resolved


__all__ = [
    "SandboxCredentialError",
    "build_sandbox_env",
]
