"""Per-phase LLM provider resolution for the turn engine.

The turn engine (Plan / Execute / Review) allows each phase to run on a
different LLM. This module is the single place that resolves which
``LLMProvider`` instance a phase should use, given a ``Role`` and the
configured provider map.

Kept as a standalone module -- not a method on ``AgentDefinition``
(which is cached and static and knows nothing about provider maps) and
not a method on ``TurnContext`` (which is per-turn state and should not
own stateless lookup logic).

Fallback order, in priority:
    1. ``role.llm_<phase>`` (e.g. ``role.llm_plan``) if set,
    2. ``role.llm`` (the string-shaped config),
    3. ``"default"`` key in the provider map,
    4. the first provider in the map.
"""

from __future__ import annotations

from typing import Literal

from crewlet._logging import get_logger
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider

logger = get_logger("agent.phase_model")

Phase = Literal[
    "plan", "execute", "review", "subagent", "auxiliary", "judge", "sandbox"
]


def _phase_key_from_role(role: Role, phase: Phase) -> str | list[str] | None:
    """Return the role's per-phase key/chain (or None)."""
    match phase:
        case "plan":
            return role.llm_plan
        case "execute":
            return role.llm_execute
        case "review":
            return role.llm_review
        case "subagent":
            return role.llm_subagent
        case "auxiliary":
            return role.llm_auxiliary
        case "judge":
            return role.llm_judge
        case "sandbox":
            # The sandboxed Execute backend's coding agent defaults to the
            # role's Execute provider -- ``llm_sandbox``
            # overrides; both then fall back to ``role.llm``.
            return role.llm_sandbox or role.llm_execute


def _as_list(value: str | list[str] | None) -> list[str]:
    """Normalise a ``str | list[str] | None`` field into an ordered
    list of non-empty strings."""
    if not value:
        return []
    if isinstance(value, str):
        return [value]
    return [v for v in value if v]


def resolve_phase_chain(
    role: Role,
    phase: Phase,
    providers: dict[str, LLMProvider],
) -> list[tuple[str, LLMProvider]]:
    """Resolve the ordered chain of ``(provider_key, provider)`` for
    ``phase`` on ``role``.

    The role's ``llm_<phase>`` -- or ``llm`` if the per-phase override
    is unset -- can be either a single string or a list of strings.
    The returned chain follows that ordering with unknown / empty
    keys dropped. When every candidate misses the providers map, we
    fall back to ``"default"`` and finally the first provider in
    insertion order, mirroring :func:`resolve_phase_provider`.

    Always returns a non-empty list. Raises ``RuntimeError`` only
    when ``providers`` is empty (a configuration error).
    """
    if not providers:
        msg = "No LLM providers configured"
        raise RuntimeError(msg)

    per_phase = _phase_key_from_role(role, phase)
    candidate_keys = _as_list(per_phase) or _as_list(role.llm)
    if not candidate_keys:
        candidate_keys = ["default"]

    chain: list[tuple[str, LLMProvider]] = []
    seen: set[str] = set()
    for key in candidate_keys:
        if key in seen:
            continue
        if key in providers:
            chain.append((key, providers[key]))
            seen.add(key)
    if chain:
        logger.debug(
            "phase_chain_resolved",
            role=role.name,
            phase=phase,
            keys=[k for k, _ in chain],
        )
        return chain
    # No candidate matched -- fall back to ``"default"`` then first.
    if "default" in providers:
        logger.debug(
            "phase_chain_fallback_default",
            role=role.name,
            phase=phase,
            tried=candidate_keys,
        )
        return [("default", providers["default"])]
    first_key = next(iter(providers))
    logger.debug(
        "phase_chain_fallback_first",
        role=role.name,
        phase=phase,
        tried=candidate_keys,
        fallback_key=first_key,
    )
    return [(first_key, providers[first_key])]


def resolve_phase_provider(
    role: Role,
    phase: Phase,
    providers: dict[str, LLMProvider],
) -> tuple[str, LLMProvider]:
    """Return the head of the phase chain. Convenience surface for
    callers that only need a single provider (i.e. don't want
    fallback). Fallback-chain callers should use
    :func:`resolve_phase_chain` instead.
    """
    return resolve_phase_chain(role, phase, providers)[0]
