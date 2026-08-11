"""``crewlet confluence`` CLI handlers — unified ``import`` and ``resync``.

``import`` publishes BOTH tool-skill pages AND general knowledge docs to
Confluence, routing each markdown file by its frontmatter ``trigger``:

- a file whose frontmatter has a ``trigger`` is a **Tool Skill** →
  published to the Tool Skills space with the YAML ``code`` macro the
  engine parses back out.  Its directory is ignored.
- every other file is a **knowledge doc** → published as clean prose to
  the space named by its **immediate parent directory** (a file at
  ``<root>/LEAD/onboarding.md`` lands in space ``LEAD``), titled by its
  first ``# H1`` (or a frontmatter ``title`` override), and surfaced via
  the query-time ``## Relevant knowledge`` search.  A doc with no
  determinable title is skipped with a warning.

``resync`` is skills-registry-only: it re-runs the boot-time full
populate of the Tool Skills registry and reports the loaded keys.

Both build a temporary :class:`ConfluenceTransport` from the supplied
Tier B company YAML and use its authenticated HTTP client directly.
Neither needs the full engine, database, or queue.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path
from typing import Any

import httpx

from crewlet._env import load_env_file
from crewlet._logging import get_logger
from crewlet.agent.skills.confluence_codec import encode_page
from crewlet.agent.skills.labels import SKILL_MARKER_LABEL
from crewlet.agent.skills.models import SKILL_REQUIRED_DEFAULT
from crewlet.agent.skills.parser import parse_skill_file
from crewlet.agent.skills.registry import PromptSkillRegistry
from crewlet.agent.skills.sync import ToolSkillSyncWorker
from crewlet.confluence import pages as cpages
from crewlet.confluence.knowledge import (
    DOC_MARKER_LABEL,
    doc_key_label,
    encode_doc,
)
from crewlet.confluence.pages import ConfluencePublishError
from crewlet.knowledge.markdown_docs import (
    DocParseError,
    classify,
    collect_md_files,
    parse_doc_file,
    split_optional_frontmatter,
)

logger = get_logger("confluence.import")


def _resolve_skill_space(arg_space: str | None) -> str:
    """Tool Skills space precedence: CLI flag > env var > default."""
    if arg_space:
        return arg_space
    return os.environ.get("CREWLET_TOOL_SKILLS_SPACE", "TS")


def _skill_key_label(skill_key: str) -> str:
    """Per-skill key label."""
    return cpages.safe_label(skill_key, prefix=f"{SKILL_MARKER_LABEL}-key")


# ---------------------------------------------------------------------------
# Lookups
# ---------------------------------------------------------------------------


async def _find_existing_skill_page(
    transport: Any,
    space_key: str,
    skill_key: str,
    *,
    title: str | None = None,
) -> dict[str, Any] | None:
    """Look up a skill page by key label, falling back to title.

    Two-stage lookup:

    1. **By label** — the cheap, rename-stable path (the page was created
       with a ``crewlet-skill-key-<key>`` label).
    2. **By title** (fallback) — recovers pages created manually or
       whose label POST failed silently.  Without it
       the importer would try to create a duplicate-title page (400).
    """
    label = _skill_key_label(skill_key)
    page = await cpages.find_by_label(transport, space_key, label)
    if page is not None:
        return page
    if title is None:
        return None
    return await cpages.find_by_title(transport, space_key, title)


async def _import_skill(
    transport: Any,
    md_path: Path,
    frontmatter: dict[str, Any],
    body: str,
    *,
    update: bool,
    dry_run: bool,
    skill_space: str,
) -> None:
    """Publish one Tool Skill page."""
    skill_key = str(frontmatter.get("key", "")).strip()
    title = str(frontmatter.get("title", md_path.stem)).strip()
    if not skill_key:
        logger.warning(
            "skill_import_skipped",
            reason="missing_key",
            source=md_path.name,
        )
        return

    # Stamp the effective enforcement state into the page's YAML box.
    # The Confluence page is the operator's editing surface; a source
    # file that omits ``required`` would otherwise create a page whose
    # metadata hides that the engine enforces the skill (the model
    # default is ``required: true``).
    frontmatter.setdefault("required", SKILL_REQUIRED_DEFAULT)
    storage = encode_page(frontmatter, body)
    labels = [SKILL_MARKER_LABEL, _skill_key_label(skill_key)]
    existing = await _find_existing_skill_page(
        transport, skill_space, skill_key, title=title
    )
    if existing is None:
        if dry_run:
            logger.info(
                "skill_imported",
                action="would_create",
                key=skill_key,
                source=md_path.name,
            )
            return
        page_id = await cpages.create_page(
            transport, skill_space, title, storage, labels=labels
        )
        logger.info(
            "skill_imported",
            action="created",
            key=skill_key,
            page_id=page_id,
            source=md_path.name,
        )
        return
    if not update:
        logger.info(
            "skill_imported",
            action="skipped",
            key=skill_key,
            page_id=existing["id"],
            source=md_path.name,
            hint="pass --update to overwrite",
        )
        return
    if dry_run:
        logger.info(
            "skill_imported",
            action="would_update",
            key=skill_key,
            page_id=existing["id"],
            source=md_path.name,
        )
        return
    page_id = await cpages.update_page(transport, existing, title, storage)
    # Re-attach labels on every update. Idempotent on Confluence's side
    # and self-heals pages that landed without their label (manual page
    # creation, transient label-POST failures) — so
    # the next run finds them via the fast label lookup.
    if page_id:
        await cpages.attach_labels(transport, page_id, labels)
    logger.info(
        "skill_imported",
        action="updated",
        key=skill_key,
        page_id=page_id,
        source=md_path.name,
    )


async def _resolve_doc_parent(
    transport: Any,
    frontmatter: dict[str, Any],
    *,
    space: str,
    title: str,
    source: str,
) -> str | None:
    """Resolve a doc's optional frontmatter ``parent`` to a page id.

    ``parent`` names an existing page by exact title in the doc's own
    target space.  Returns ``None`` when no parent is declared or the
    named page doesn't exist — the latter logs a hint and the doc lands
    at the space root rather than failing the import (the parent may
    simply not be published yet; a later create won't retroactively
    nest this page, so the log tells the operator what happened).
    """
    parent_title = str(frontmatter.get("parent", "") or "").strip()
    if not parent_title:
        return None
    parent_page = await cpages.find_by_title(transport, space, parent_title)
    if parent_page is not None:
        return str(parent_page.get("id", "")) or None
    logger.info(
        "doc_parent_not_found",
        parent=parent_title,
        space=space,
        title=title,
        source=source,
        hint="creating at the space root; publish or create the parent "
        "page first to nest new docs under it",
    )
    return None


async def _import_doc(
    transport: Any,
    md_path: Path,
    frontmatter: dict[str, Any],
    body: str,
    *,
    space: str,
    title: str,
    update: bool,
    dry_run: bool,
) -> None:
    """Publish one knowledge doc as clean prose into its target space.

    ``space`` is derived from the file's parent directory and ``title``
    from the first ``# H1`` (or a frontmatter override) by the caller.
    A frontmatter ``parent`` (a page title) nests a **newly created**
    page under that parent, resolved by exact title in the target space;
    a missing parent falls back to the space root with a hint log.  An
    *existing* page is never re-parented — operators own the position of
    pages already in the space.
    """
    storage = encode_doc(body)
    labels = [DOC_MARKER_LABEL, doc_key_label(space, title)]
    extra = frontmatter.get("labels") or []
    if isinstance(extra, list):
        labels.extend(str(label) for label in extra)
    existing = await cpages.find_by_title(transport, space, title)
    if existing is None:
        parent_id = await _resolve_doc_parent(
            transport, frontmatter, space=space, title=title, source=md_path.name
        )
        if dry_run:
            logger.info(
                "doc_imported",
                action="would_create",
                title=title,
                space=space,
                source=md_path.name,
            )
            return
        page_id = await cpages.create_page(
            transport, space, title, storage, labels=labels, parent_id=parent_id
        )
        logger.info(
            "doc_imported",
            action="created",
            title=title,
            space=space,
            page_id=page_id,
            source=md_path.name,
        )
        return
    if not update:
        logger.info(
            "doc_imported",
            action="skipped",
            title=title,
            space=space,
            page_id=existing["id"],
            source=md_path.name,
            hint="pass --update to overwrite",
        )
        return
    if dry_run:
        logger.info(
            "doc_imported",
            action="would_update",
            title=title,
            space=space,
            page_id=existing["id"],
            source=md_path.name,
        )
        return
    page_id = await cpages.update_page(transport, existing, title, storage)
    if page_id:
        await cpages.attach_labels(transport, page_id, labels)
    logger.info(
        "doc_imported",
        action="updated",
        title=title,
        space=space,
        page_id=page_id,
        source=md_path.name,
    )


async def import_one(
    transport: Any,
    md_path: Path,
    *,
    update: bool,
    dry_run: bool,
    skill_space: str,
) -> None:
    """Process one markdown file: parse, classify, dispatch.

    Emits a structured log event per outcome, uniform with the rest of
    the engine's structlog stream.  Malformed / untitled files are
    logged and skipped — they never raise out of the loop.
    """
    text = md_path.read_text()
    # Classify on the tolerant split (knowledge docs are frontmatter-less
    # under the directory convention, so a missing block is not an error
    # here — it just routes the file to the doc branch).
    try:
        frontmatter, _body = split_optional_frontmatter(text)
    except DocParseError as exc:
        logger.warning(
            "confluence_import_skipped",
            reason="invalid_frontmatter",
            source=md_path.name,
            error=str(exc),
        )
        return

    if classify(frontmatter) == "skill":
        # Re-parse through the canonical skill parser so the body is
        # stripped and frontmatter validated exactly as on the
        # skills-only path — both paths publish an identical page.
        skill_frontmatter, skill_body = parse_skill_file(text)
        await _import_skill(
            transport,
            md_path,
            skill_frontmatter,
            skill_body,
            update=update,
            dry_run=dry_run,
            skill_space=skill_space,
        )
        return

    # kind == "doc": space from the parent directory, title from the
    # first H1 (or a frontmatter override) with that H1 stripped.
    space = md_path.parent.name
    try:
        fm, title, doc_body = parse_doc_file(text)
    except DocParseError as exc:
        logger.warning(
            "confluence_import_skipped",
            reason="invalid_doc",
            source=md_path.name,
            error=str(exc),
        )
        return
    await _import_doc(
        transport,
        md_path,
        fm,
        doc_body,
        space=space,
        title=title,
        update=update,
        dry_run=dry_run,
    )


# ---------------------------------------------------------------------------
# Shared async core
# ---------------------------------------------------------------------------


def _target_space(
    md_path: Path, frontmatter: dict[str, Any], *, skill_space: str
) -> str:
    """Resolve a file's target Confluence space without publishing.

    Skill files (``trigger`` in frontmatter) land in ``skill_space``;
    knowledge docs land in the space named by their **parent directory**.
    Used by the pre-flight pass to enumerate every distinct space before
    any page is written.
    """
    if classify(frontmatter) == "skill":
        return skill_space
    return md_path.parent.name


async def _prune_orphan_skills(
    transport: Any,
    skill_space: str,
    local_keys: set[str],
    *,
    dry_run: bool,
) -> None:
    """Delete import-managed skill pages whose source ``.md`` was removed.

    Targets ONLY pages carrying ``SKILL_MARKER_LABEL`` (the importer's own
    marker), so user-authored pages without it are never touched. A skill
    page is an orphan when its per-key label is not among the set the local
    files would publish; a marker page with no recognizable key label is
    left alone (ambiguous). Knowledge docs are out of scope -- they are
    content, not config, and span many spaces.
    """
    local_labels = {_skill_key_label(k) for k in local_keys}
    key_prefix = f"{SKILL_MARKER_LABEL}-key"
    pages = await cpages.find_all_by_label(transport, skill_space, SKILL_MARKER_LABEL)
    for page in pages:
        labels = {
            str(lab.get("name", ""))
            for lab in (
                page.get("metadata", {}).get("labels", {}).get("results", []) or []
            )
        }
        key_labels = {lab for lab in labels if lab.startswith(key_prefix)}
        if not key_labels or not key_labels.isdisjoint(local_labels):
            continue  # not identifiable as a skill, or its key is still local
        page_id = str(page.get("id", ""))
        title = str(page.get("title", ""))
        if dry_run:
            logger.info(
                "skill_pruned", action="would_delete", page_id=page_id, title=title
            )
            continue
        if await cpages.delete_page(transport, page_id):
            logger.info("skill_pruned", action="deleted", page_id=page_id, title=title)


async def run_import(
    *,
    config_path: Path,
    paths: list[Path],
    update: bool = False,
    dry_run: bool = False,
    create_space: bool = False,
    prune: bool = False,
    skill_space: str,
) -> int:
    """Shared async core for the unified import flow.

    Used by ``cmd_confluence_import`` and by ``cmd_run`` (when invoked
    with ``--import-confluence``) so the same code path seeds Confluence
    whether the operator runs the import as a one-shot or bundles it with
    the engine start.

    Collects every ``*.md`` under ``paths`` (recursively), routes each by
    frontmatter, pre-flights every distinct target space, then publishes.
    Returns 0 on success, non-zero on failure.

    ``create_space=True`` auto-creates any missing target Confluence
    space found during the per-space pre-flight.  Requires the bot
    account to have Confluence space-admin permission on the tenant.

    ``prune=True`` deletes import-managed skill pages in ``skill_space``
    whose source ``.md`` is no longer present (e.g. a renamed/removed
    bundled skill) — only pages the importer itself published, never
    user-authored ones. Knowledge docs are not pruned.
    """
    files = collect_md_files(paths)
    transport = await cpages.build_transport(config_path)
    try:
        # Split each file's optional frontmatter once to determine target
        # spaces.  Knowledge docs are frontmatter-less under the directory
        # convention, so use the tolerant splitter (not parse_skill_file)
        # — otherwise every plain-prose doc would drop out of the
        # pre-flight set.  Malformed files are left for import_one to log
        # and skip; they contribute no target space here.
        target_spaces: set[str] = set()
        local_skill_keys: set[str] = set()
        for md_path in files:
            try:
                frontmatter, _body = split_optional_frontmatter(md_path.read_text())
            except DocParseError:
                continue
            target_spaces.add(
                _target_space(md_path, frontmatter, skill_space=skill_space)
            )
            if classify(frontmatter) == "skill":
                key = str(frontmatter.get("key", "")).strip()
                if key:
                    local_skill_keys.add(key)

        # Per-space pre-flight: fail fast with a useful message when the
        # operator points at a space that doesn't exist.  The bulk loop's
        # first call would otherwise surface as a bare 404 with no hint of
        # what to fix.
        for space in sorted(target_spaces):
            exists = await cpages.check_space_exists(transport, space)
            if exists is False:
                if create_space:
                    if dry_run:
                        logger.info("confluence_space_would_be_created", space=space)
                    else:
                        await cpages.create_space(transport, space)
                        logger.info("confluence_space_created", space=space)
                else:
                    raise ConfluencePublishError(
                        f"Confluence space '{space}' not found. Pass "
                        f"--create-space to have the import auto-create "
                        f"it, create the space in Confluence first (Space "
                        f"directory → Create space; pick any name, set "
                        f"the key to '{space}'), or set the right space "
                        f"(--space / CREWLET_TOOL_SKILLS_SPACE for skills; "
                        f"the parent directory name for knowledge docs)."
                    )

        logger.info(
            "confluence_import_started",
            count=len(files),
            spaces=sorted(target_spaces),
            update=update,
            dry_run=dry_run,
        )
        try:
            for md_path in files:
                await import_one(
                    transport,
                    md_path,
                    update=update,
                    dry_run=dry_run,
                    skill_space=skill_space,
                )
            if prune:
                await _prune_orphan_skills(
                    transport, skill_space, local_skill_keys, dry_run=dry_run
                )
        except httpx.HTTPError as exc:
            # Surface a friendly message instead of a raw stack trace for
            # any Confluence failure (auth, permissions, connectivity).
            cred_hint = (
                " — check the Confluence credentials (email + API token) in "
                "the company config's 'confluence:' block and that the account "
                "can write to the space"
            )
            if isinstance(exc, httpx.HTTPStatusError):
                status = exc.response.status_code
                detail = f"returned HTTP {status}"
                hint = cred_hint if status in (401, 403) else ""
            else:
                detail = f"request failed ({exc})"
                hint = cred_hint
            raise ConfluencePublishError(
                f"Confluence {detail} during import{hint}"
            ) from exc
        logger.info(
            "confluence_import_completed",
            count=len(files),
            spaces=sorted(target_spaces),
        )
    finally:
        await transport.stop()
    return 0


# ---------------------------------------------------------------------------
# Import command
# ---------------------------------------------------------------------------


def cmd_confluence_import(args: Any) -> int:
    """Publish local skill + knowledge-doc markdown into Confluence."""
    config_path: Path = args.config
    # Honour a project .env the same way ``crewlet run`` does so the
    # Confluence credentials referenced by the company YAML resolve from
    # the environment even when they live only in .env.
    load_env_file(config_path)
    if not config_path.exists():
        logger.error("confluence_import_config_missing", path=str(config_path))
        return 1
    path: Path = args.path
    if not path.exists():
        logger.error("confluence_import_path_missing", path=str(path))
        return 1
    skill_space = _resolve_skill_space(args.space)

    try:
        return asyncio.run(
            run_import(
                config_path=config_path,
                paths=[path],
                update=bool(args.update),
                dry_run=bool(args.dry_run),
                create_space=bool(getattr(args, "create_space", False)),
                prune=bool(getattr(args, "prune", False)),
                skill_space=skill_space,
            )
        )
    except ConfluencePublishError as exc:
        logger.error("confluence_import_failed", error=str(exc))
        return 1
    except (RuntimeError, ValueError) as exc:
        logger.error("confluence_import_failed", error=str(exc))
        return 1


# ---------------------------------------------------------------------------
# Resync command (skills registry only)
# ---------------------------------------------------------------------------


def cmd_confluence_resync(args: Any) -> int:
    """Re-fetch the Tool Skills space and dump what would be loaded.

    This is a diagnostic / drift-recovery tool. It uses a *temporary*
    PromptSkillRegistry isolated from any running engine: a live engine
    receives Confluence webhook events directly, so a manual resync is
    only needed when an operator suspects a webhook was missed across a
    long outage. The CLI re-runs the same boot-time full populate code
    path and reports counts; restart the running engine (or wait for the
    next webhook) to apply changes there.

    Resync is skills-registry-only — knowledge docs are searched live and
    are never loaded into a registry, so there is nothing to resync for
    them.
    """
    config_path: Path = args.config
    load_env_file(config_path)
    if not config_path.exists():
        logger.error("skill_resync_config_missing", path=str(config_path))
        return 1
    space_key = _resolve_skill_space(args.space)

    async def _run() -> int:
        transport = await cpages.build_transport(config_path)
        registry = PromptSkillRegistry()
        worker = ToolSkillSyncWorker(
            transport=transport,
            registry=registry,
            space_key=space_key,
        )
        try:
            count = await worker.run_initial_sync()
        finally:
            await transport.stop()
        if count is None:
            # The walk failed (unreachable Confluence, non-200 mid-walk)
            # — a diagnostic that prints an empty key list as if the
            # space were empty would mislead; fail loudly instead.
            logger.error("skill_resync_failed", space=space_key, reason="walk_failed")
            return 1
        logger.info(
            "skill_resync_completed",
            space=space_key,
            count=count,
            keys=registry.keys(),
        )
        return 0

    try:
        return asyncio.run(_run())
    except ConfluencePublishError as exc:
        logger.error("skill_resync_failed", error=str(exc))
        return 1
    except (RuntimeError, ValueError) as exc:
        logger.error("skill_resync_failed", error=str(exc))
        return 1


__all__ = [
    "cmd_confluence_import",
    "cmd_confluence_resync",
    "import_one",
    "run_import",
]
