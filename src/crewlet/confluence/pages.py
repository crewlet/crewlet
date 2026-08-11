"""Generic Confluence page operations — zero skill/doc specifics.

A small async wrapper over the Confluence Cloud v1 REST API, built on
the authenticated httpx client a :class:`ConfluenceTransport` already
exposes (``_http_client`` + ``_rest_base``).  These helpers know how to
find / create / update pages, attach labels, and create spaces; they
know nothing about Tool Skills or knowledge docs.  The unified import
CLI (:mod:`crewlet.confluence.import_cli`) layers the skill / doc
routing on top of this shared surface.

Every helper takes a ``transport`` typed as ``Any`` so callers can pass
either the real :class:`ConfluenceTransport` or a lightweight test
double exposing the same two attributes.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

from crewlet._logging import get_logger
from crewlet.config import (
    ConfluenceConfig,
    _resolve_env_recursive,
    load_company_config,
)

logger = get_logger("confluence.pages")


class ConfluencePublishError(RuntimeError):
    """Raised to surface a friendly operator-facing publish failure.

    Wraps the upstream Confluence error in a message that tells the
    operator what to do (create the space, fix credentials, etc.) so
    they don't have to read a stack trace to recover.
    """


async def build_transport(config_path: Path) -> Any:
    """Load the YAML config and return a started ConfluenceTransport.

    Caller is responsible for stopping it.
    """
    config = load_company_config(config_path)
    if config.integrations.confluence is None:
        raise ValueError(
            "config does not declare a Confluence integration "
            "('integrations.confluence' block); nothing to do"
        )
    # ``load_company_config`` keeps ``${VAR}`` references verbatim (the
    # engine resolves them in its builders); this CLI path doesn't go
    # through them, so resolve the Confluence credentials against the
    # environment here.  Otherwise the transport authenticates with the
    # literal "${JIRA_ADMIN_API_TOKEN}" / "${JIRA_ADMIN_EMAIL}" strings
    # and Confluence rejects every request with 401.
    confluence = ConfluenceConfig.model_validate(
        _resolve_env_recursive(
            config.integrations.confluence.model_dump(mode="python", exclude_none=True)
        )
    )
    # Fail fast with a precise message when the resolved credentials are
    # empty -- otherwise the transport sends an empty auth header and the
    # operator gets an opaque protocol/401 error on every request.
    if not confluence.token:
        ref = config.integrations.confluence.token
        if ref.startswith("${") and ref.endswith("}"):
            detail = f"the {ref[2:-1]!r} environment variable is unset or empty"
        else:
            detail = "no token is configured in the 'confluence:' block"
        raise ConfluencePublishError(
            f"Confluence credentials are not available: {detail}. Set it in "
            "your environment (or .env) before publishing pages."
        )
    # Imported lazily so the unit tests don't have to drag the full
    # transport graph into the CLI module import path.
    from crewlet.notifications.transports.confluence import ConfluenceTransport

    transport = ConfluenceTransport(confluence)
    await transport.start()
    return transport


# ---------------------------------------------------------------------------
# Labels
# ---------------------------------------------------------------------------


_INVALID_LABEL_CHAR_RE = re.compile(r"[^a-z0-9_-]")


def safe_label(value: str, *, prefix: str) -> str:
    """Encode an arbitrary string into a Confluence-label-safe string.

    Lowercases the value (Confluence stores labels lowercase anyway) and
    replaces any char outside ``[a-z0-9_-]`` with ``_``.  The mapping is
    one-way — we never decode labels back into values; we only search by
    the produced label string.  Returns ``f"{prefix}-{safe}"``.
    """
    safe = _INVALID_LABEL_CHAR_RE.sub("_", value.lower())
    return f"{prefix}-{safe}"


# ---------------------------------------------------------------------------
# Lookups
# ---------------------------------------------------------------------------


async def find_by_label(
    transport: Any, space_key: str, label: str
) -> dict[str, Any] | None:
    """Look up a page in ``space_key`` carrying ``label`` via CQL.

    The cheap, rename-stable lookup path: pages are stamped with a
    per-entity key label on create, so this finds them again regardless
    of any title change.  Returns the first match or ``None``.
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base:
        return None
    cql = f'space="{space_key}" AND label="{label}"'
    url = f"{base}/rest/api/content/search?cql={cql}&expand=version&limit=2"
    try:
        resp = await client.get(url)
    except Exception:
        logger.exception("confluence_label_search_failed", label=label)
        return None
    if resp.status_code == 200:
        results = resp.json().get("results", []) or []
        if results:
            return results[0]
        return None
    logger.warning(
        "confluence_label_search_non_200",
        label=label,
        status=resp.status_code,
    )
    return None


async def find_all_by_label(
    transport: Any, space_key: str, label: str, *, limit: int = 200
) -> list[dict[str, Any]]:
    """List every page in ``space_key`` carrying ``label`` (CQL).

    Expands each page's labels so callers can match per-entity key labels
    (used by the import's ``--prune`` to find orphaned skill pages).
    Returns ``[]`` when the transport is unconfigured or the search fails
    — a prune that can't enumerate must delete nothing, not guess.
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base:
        return []
    cql = f'space="{space_key}" AND label="{label}"'
    url = (
        f"{base}/rest/api/content/search?cql={cql}"
        f"&expand=metadata.labels&limit={int(limit)}"
    )
    try:
        resp = await client.get(url)
    except Exception:
        logger.exception("confluence_label_list_failed", label=label)
        return []
    if resp.status_code != 200:
        logger.warning(
            "confluence_label_list_non_200", label=label, status=resp.status_code
        )
        return []
    return resp.json().get("results", []) or []


async def delete_page(transport: Any, page_id: str) -> bool:
    """Delete a Confluence page by id. Returns ``True`` on success.

    Best-effort: a transport-unconfigured / failed delete returns ``False``
    (logged) rather than raising, so a bulk prune keeps going.
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base or not page_id:
        return False
    try:
        resp = await client.delete(f"{base}/rest/api/content/{page_id}")
    except Exception:
        logger.exception("confluence_page_delete_failed", page_id=page_id)
        return False
    if resp.status_code in (200, 204):
        return True
    logger.warning(
        "confluence_page_delete_non_200", page_id=page_id, status=resp.status_code
    )
    return False


async def find_by_title(
    transport: Any, space_key: str, title: str
) -> dict[str, Any] | None:
    """Look up an existing page in ``space_key`` by exact ``title``."""
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base:
        return None
    params = {
        "spaceKey": space_key,
        "title": title,
        "type": "page",
        "expand": "version",
        "limit": "2",
    }
    try:
        resp = await client.get(f"{base}/rest/api/content", params=params)
    except Exception:
        logger.exception("confluence_title_search_failed", title=title)
        return None
    if resp.status_code != 200:
        logger.warning(
            "confluence_title_search_non_200",
            title=title,
            status=resp.status_code,
        )
        return None
    results = resp.json().get("results", []) or []
    return results[0] if results else None


async def check_space_exists(transport: Any, space_key: str) -> bool | None:
    """Pre-flight: confirm the target Confluence space is reachable.

    Returns:
      - ``True`` if the space exists and is reachable.
      - ``False`` if the API explicitly says the space is missing (404
        on the space lookup).
      - ``None`` if the check couldn't run (transport unconfigured,
        non-404 error) — fall through and let the per-page calls
        surface whatever the real failure is.
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base:
        return None
    try:
        resp = await client.get(f"{base}/rest/api/space/{space_key}")
    except Exception:
        return None
    if resp.status_code == 200:
        return True
    if resp.status_code == 404:
        return False
    return None


# ---------------------------------------------------------------------------
# Mutations
# ---------------------------------------------------------------------------


async def create_space(
    transport: Any,
    space_key: str,
    *,
    name: str | None = None,
    description: str | None = None,
) -> None:
    """Create a Confluence space via the v1 REST API.

    Raises :class:`ConfluencePublishError` on a non-200 response so the
    caller can surface a friendly message (most failures here are
    permission-related — the bot account needs Confluence space-admin
    rights on the tenant).
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base:
        raise ConfluencePublishError(
            "Confluence transport not configured — can't create the space."
        )
    body = {
        "key": space_key,
        "name": name or f"Crewlet ({space_key})",
        "description": {
            "plain": {
                "value": description or "Crewlet-managed pages.",
                "representation": "plain",
            }
        },
    }
    try:
        resp = await client.post(f"{base}/rest/api/space", json=body)
    except Exception as exc:
        raise ConfluencePublishError(
            f"Failed to call Confluence to create space '{space_key}': {exc}"
        ) from exc
    if resp.status_code == 200:
        return
    if resp.status_code in {401, 403}:
        raise ConfluencePublishError(
            f"Confluence rejected the request to create space "
            f"'{space_key}' ({resp.status_code}). The bot account "
            f"needs Confluence space-admin permission on the tenant. "
            f"Either grant the permission and retry, or create the "
            f"space manually and re-run without --create-space."
        )
    raise ConfluencePublishError(
        f"Confluence returned {resp.status_code} creating space "
        f"'{space_key}': {(resp.text or '')[:200]}"
    )


async def create_page(
    transport: Any,
    space_key: str,
    title: str,
    storage_xhtml: str,
    *,
    labels: list[str],
    parent_id: str | None = None,
) -> str:
    """Create a page in ``space_key`` and attach ``labels``.

    Labels are set in a follow-up POST to ``/content/{id}/label`` rather
    than via ``metadata.labels`` on create — Cloud's v1 parser is
    inconsistent about accepting them inline, returning a bare 400
    without naming the offending field.

    ``parent_id`` nests the new page under an existing one (the v1
    ``ancestors`` field — Confluence derives the full chain from the
    single direct parent).  ``None`` creates at the space root.

    Returns the new page id.  Raises :class:`ConfluencePublishError`
    with a friendly message on the common 404 (missing space) / 400
    (storage / title rejection) responses.
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    body: dict[str, Any] = {
        "type": "page",
        "title": title,
        "space": {"key": space_key},
        "body": {"storage": {"value": storage_xhtml, "representation": "storage"}},
    }
    if parent_id:
        body["ancestors"] = [{"id": parent_id}]
    resp = await client.post(f"{base}/rest/api/content", json=body)
    if resp.status_code == 404:
        # v1 Confluence returns 404 on the content endpoint when the
        # referenced space doesn't exist. Re-raise with a clearer
        # message so operators know to create the space first.
        raise ConfluencePublishError(
            f"Confluence returned 404 creating page '{title}' in space "
            f"'{space_key}'. The space likely doesn't exist — create "
            f"it in Confluence first (Space directory → Create space), "
            f"or pass --space <existing key>."
        )
    if resp.status_code == 400:
        # Surface Confluence's reason — it usually names the field
        # that was rejected (invalid storage format, duplicate title,
        # title too long, etc.).
        raise ConfluencePublishError(
            f"Confluence rejected the page create for '{title}' "
            f"(HTTP 400): {(resp.text or '')[:500]}"
        )
    resp.raise_for_status()
    page_id = str(resp.json().get("id", ""))
    if page_id:
        await attach_labels(transport, page_id, labels)
    return page_id


async def update_page(
    transport: Any,
    page: dict[str, Any],
    title: str,
    storage_xhtml: str,
) -> str:
    """Update an existing page's title + body, bumping the version."""
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    page_id = page["id"]
    current_version = int(page.get("version", {}).get("number", 1))
    body = {
        "id": page_id,
        "type": "page",
        "title": title,
        "version": {"number": current_version + 1},
        "body": {"storage": {"value": storage_xhtml, "representation": "storage"}},
    }
    resp = await client.put(f"{base}/rest/api/content/{page_id}", json=body)
    resp.raise_for_status()
    return str(page_id)


async def attach_labels(transport: Any, page_id: str, labels: list[str]) -> None:
    """POST labels onto an existing page.

    Best-effort: a label failure logs a warning but doesn't fail the
    overall publish — operators can re-run with ``--update`` to retry,
    or add the label manually.  Without the key label, the next
    ``--update`` won't find the page by its key (it'll create a
    duplicate), so we surface a warning so the operator knows.
    """
    client = getattr(transport, "_http_client", None)
    base = getattr(transport, "_rest_base", "") or ""
    if client is None or not base or not labels:
        # An empty label list has nothing to attach — POSTing ``[]``
        # would just earn a Confluence 400 and a spurious warning
        # (the label-less promotion-draft path passes ``labels=[]``).
        return
    body = [{"prefix": "global", "name": name} for name in labels]
    try:
        resp = await client.post(f"{base}/rest/api/content/{page_id}/label", json=body)
    except Exception:
        logger.warning("confluence_label_post_failed", page_id=page_id)
        return
    if resp.status_code >= 400:
        logger.warning(
            "confluence_label_post_non_2xx",
            page_id=page_id,
            status=resp.status_code,
            body_preview=(resp.text or "")[:200],
        )


__all__ = [
    "ConfluencePublishError",
    "attach_labels",
    "build_transport",
    "check_space_exists",
    "create_page",
    "create_space",
    "find_by_label",
    "find_by_title",
    "safe_label",
    "update_page",
]
