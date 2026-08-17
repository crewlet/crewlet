"""Auto-summary generator for per-entity ``/config/*`` writes.

Per-entity routes (``PUT /config/identity``, ``POST /config/roles``,
etc.) need an audit summary so revision history reads sensibly even
when the operator didn't supply one.  This module walks the two
payloads and emits a short human-readable string.

Covers the common cases; richer diffing can be added
incrementally.  The output is a single line:

  "Added role 'Designer'"
  "Updated role 'Engineer' (goal, llm)"
  "Removed MCP server 'jira'"
  "Updated identity (mission)"
  "Updated turn_engine settings"
"""

from __future__ import annotations

from typing import Any

from crewlet.org.models import slugify


def _role_by_handle(payload: dict[str, Any], handle: str) -> dict[str, Any] | None:
    """Return a role dict in the payload by handle (root or any unit)."""
    for r in payload.get("roles", []) or []:
        if r.get("handle") == handle or slugify(r.get("name", "")) == handle:
            return r
    for u in payload.get("units", []) or []:
        found = _scan_unit(u, handle)
        if found is not None:
            return found
    return None


def _scan_unit(unit: dict[str, Any], handle: str) -> dict[str, Any] | None:
    for r in unit.get("roles", []) or []:
        if r.get("handle") == handle or slugify(r.get("name", "")) == handle:
            return r
    for child in unit.get("children", []) or []:
        found = _scan_unit(child, handle)
        if found is not None:
            return found
    return None


def summarize_change(old: dict[str, Any], new: dict[str, Any]) -> str:
    """Return a single-line audit summary for an old → new payload diff.

    Tries the entity-specific summaries first (added/removed/updated
    role / unit / mcp server / provider), then falls back to a
    coarse "Updated <section> (<changed_fields>)" line.
    """
    # Identity-only changes.  Walk the UNION of old + new keys so a
    # removed top-level section (e.g. all of ``learning:`` dropped)
    # isn't misclassified as identity-only.
    identity_fields = {"name", "mission", "vision", "policies"}
    identity_diff = [f for f in identity_fields if old.get(f) != new.get(f)]
    if identity_diff and all(
        old.get(k) == new.get(k)
        for k in (set(old) | set(new))
        if k not in identity_fields and k != "_summary"
    ):
        return f"Updated identity ({', '.join(identity_diff)})"

    # Role roster — added / removed / single-role updated.
    old_roles_by_h = _flat_role_handles(old)
    new_roles_by_h = _flat_role_handles(new)
    added_roles = new_roles_by_h - old_roles_by_h
    removed_roles = old_roles_by_h - new_roles_by_h
    if len(added_roles) == 1 and not removed_roles:
        h = next(iter(added_roles))
        return f"Added role '{_role_label(new, h)}'"
    if len(removed_roles) == 1 and not added_roles:
        h = next(iter(removed_roles))
        return f"Removed role '{_role_label(old, h)}'"
    if not added_roles and not removed_roles:
        for h in new_roles_by_h:
            old_r = _role_by_handle(old, h)
            new_r = _role_by_handle(new, h)
            if old_r is None or new_r is None:
                continue
            changed = [k for k in new_r if old_r.get(k) != new_r.get(k)]
            if changed:
                return f"Updated role '{_role_label(new, h)}' ({', '.join(changed)})"

    # MCP servers — added / removed.
    old_mcps = {m.get("name") for m in old.get("mcp_servers", []) or []}
    new_mcps = {m.get("name") for m in new.get("mcp_servers", []) or []}
    if len(new_mcps - old_mcps) == 1 and not (old_mcps - new_mcps):
        return f"Added MCP server '{next(iter(new_mcps - old_mcps))}'"
    if len(old_mcps - new_mcps) == 1 and not (new_mcps - old_mcps):
        return f"Removed MCP server '{next(iter(old_mcps - new_mcps))}'"

    # LLM provider entries.
    old_llm = set((old.get("providers") or {}).get("llm", {}).keys())
    new_llm = set((new.get("providers") or {}).get("llm", {}).keys())
    if len(new_llm - old_llm) == 1 and not (old_llm - new_llm):
        return f"Added LLM provider '{next(iter(new_llm - old_llm))}'"
    if len(old_llm - new_llm) == 1 and not (new_llm - old_llm):
        return f"Removed LLM provider '{next(iter(old_llm - new_llm))}'"

    # Top-level section bumps.
    for section in ("turn_engine", "learning", "providers"):
        if old.get(section) != new.get(section):
            return f"Updated {section}"

    # Integration sub-section bumps (jira / confluence / slack / github /
    # gitlab / plane).
    old_int = old.get("integrations") or {}
    new_int = new.get("integrations") or {}
    for kind in (
        "jira",
        "confluence",
        "slack",
        "mattermost",
        "github",
        "gitlab",
        "plane",
    ):
        if old_int.get(kind) != new_int.get(kind):
            return f"Updated {kind}"

    if old.get("token_budget") != new.get("token_budget"):
        return f"Updated org token_budget → {new.get('token_budget', 0)}"

    return "Updated config"


def _flat_role_handles(payload: dict[str, Any]) -> set[str]:
    """Collect every role's handle (or slugified name) in the payload."""
    out: set[str] = set()
    for r in payload.get("roles", []) or []:
        out.add(r.get("handle") or slugify(r.get("name", "")))
    for u in payload.get("units", []) or []:
        _collect_unit_handles(u, out)
    return out - {""}


def _collect_unit_handles(unit: dict[str, Any], out: set[str]) -> None:
    for r in unit.get("roles", []) or []:
        out.add(r.get("handle") or slugify(r.get("name", "")))
    for child in unit.get("children", []) or []:
        _collect_unit_handles(child, out)


def _role_label(payload: dict[str, Any], handle: str) -> str:
    r = _role_by_handle(payload, handle)
    if r is None:
        return handle
    return r.get("name") or handle
