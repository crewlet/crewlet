"""``crewlet plane`` CLI handlers — unified ``import`` and ``resync``.

The Plane analog of :mod:`crewlet.confluence.import_cli`.  ``import``
publishes BOTH tool-skill pages AND general knowledge docs to Plane,
routing each markdown file by its frontmatter ``trigger``:

- a file whose frontmatter has a ``trigger`` is a **Tool Skill** →
  published to the Tool Skills project with the leading YAML code block
  the engine parses back out (:mod:`crewlet.plane.plane_codec`).  Its
  directory is ignored.
- every other file is a **knowledge doc** → published as clean prose to
  the project whose identifier is its **immediate parent directory**
  name (a file at ``<root>/ENG/onboarding.md`` lands in project
  ``ENG``), titled by its first ``# H1`` (or a frontmatter ``title``
  override), and surfaced via the query-time ``## Relevant knowledge``
  search.  A doc with no determinable title is skipped with a warning.

Idempotency is the fork's ``external_id`` / ``external_source``
contract (the fork enforces project-wide uniqueness of the pair under an
advisory lock and 409s on a duplicate): every page the importer
publishes is stamped ``external_source="crewlet"`` with
``external_id="skill:<key>"`` (skills) or ``external_id="doc:<title>"``
(docs, keyed by the authored H1).  One ``list_pages`` enumeration per
target project — fetched with a narrow ``fields=`` projection — is
indexed by external id; retitling a page in Plane's UI never orphans it
because the external identity, not the title, is the match key.  An
exact-title fallback adopts pre-existing unmarked pages (and
``--update`` stamps it onto them).

Failures are isolated per page: a locked page or a page-level 403 logs
``plane_page_write_failed`` and the run keeps publishing the remaining
files, then exits non-zero naming the failures (only an invalid
credential — 401 — or a pre-write enumeration failure aborts outright).

``--prune`` deletes orphaned **skill** pages only, on a positive-marker
predicate: ``external_source == "crewlet"`` AND a ``skill:`` external
id whose key no local file publishes.  Unmarked pages, ``doc:`` pages,
and knowledge docs are never touched, and deletion follows the fork's
archive-then-delete precondition with per-page failure isolation; when
the archive lands but the delete is refused (deletion is
owner-or-project-admin only) the archive is rolled back so a failed
prune is a true no-op rather than a page stranded archived behind a
permanently 409ing external id.

``resync`` is skills-registry-only: it re-runs the boot-time full
populate of the Tool Skills registry and reports the loaded keys.

Both build a :class:`~crewlet.plane.client.PlaneClient` from the
supplied Tier B company YAML (``integrations.plane``).  Neither needs
the full engine, database, or queue.
"""

from __future__ import annotations

import asyncio
import os
from pathlib import Path
from typing import Any

from crewlet._env import load_env_file
from crewlet._logging import get_logger
from crewlet.agent.skills.models import SKILL_REQUIRED_DEFAULT
from crewlet.agent.skills.parser import parse_skill_file
from crewlet.agent.skills.registry import PromptSkillRegistry
from crewlet.knowledge.markdown_docs import (
    DocParseError,
    classify,
    collect_md_files,
    parse_doc_file,
    split_optional_frontmatter,
)
from crewlet.plane.client import PlaneClient, PlaneProvisionError
from crewlet.plane.plane_codec import encode_doc, encode_page

logger = get_logger("plane.import")

EXTERNAL_SOURCE = "crewlet"
"""``external_source`` stamped on every page the engine publishes.

The positive marker the prune predicate and the idempotency lookups key
on — the Plane analog of the Confluence ``crewlet-skill`` /
``crewlet-doc`` marker labels.
"""

_SKILL_EXTERNAL_PREFIX = "skill:"
_DOC_EXTERNAL_PREFIX = "doc:"

_INDEX_FIELDS = ["id", "name", "external_id", "external_source"]
"""``fields=`` projection for the one-per-project import enumeration.

The importer matches on external identity and exact name only — bodies
are never fetched, and the enumeration stays one narrow paginated walk
per project.
"""


class PlanePublishError(RuntimeError):
    """Raised to surface a friendly operator-facing publish failure.

    The Plane peer of
    :class:`~crewlet.confluence.pages.ConfluencePublishError`: wraps the
    upstream Plane error in a message that tells the operator what to do
    (create the project, fix the token, etc.) so they don't have to read
    a stack trace to recover.
    """


def skill_external_id(skill_key: str) -> str:
    """The ``external_id`` for a Tool Skill page (``skill:<key>``)."""
    return f"{_SKILL_EXTERNAL_PREFIX}{skill_key}"


def doc_external_id(title: str) -> str:
    """The ``external_id`` for a knowledge doc (``doc:<authored H1>``)."""
    return f"{_DOC_EXTERNAL_PREFIX}{title}"


def _resolve_skills_project(arg_project: str | None) -> str:
    """Tool Skills project precedence: CLI flag > env var > default.

    Same env var the engine reads for its skills-project selection
    (``CREWLET_TOOL_SKILLS_PROJECT``), so import, search exclusion, and
    sync all agree on one identifier.
    """
    if arg_project:
        return arg_project
    return os.environ.get("CREWLET_TOOL_SKILLS_PROJECT", "TS")


async def build_client_from_config(config_path: Path) -> PlaneClient:
    """Load the Tier B YAML and return a Plane client for its integration.

    Resolves ``${VAR}`` references for the three fields this CLI
    consumes (``url`` / ``workspace`` / ``token``) — the block already
    validated at load time with its references verbatim, and
    re-validating the resolved block would also demand a
    ``webhook_secret`` the importer never uses.  Raises
    :class:`PlanePublishError` with a clear operator message when the
    integration is absent, disabled, or credential-less.  Caller is
    responsible for closing the client.
    """
    from crewlet.config import _resolve_env_recursive, load_company_config

    config = load_company_config(config_path)
    plane = config.integrations.plane
    if plane is None or not plane.enabled:
        raise PlanePublishError(
            "config does not declare an enabled Plane integration "
            "('integrations.plane' block with enabled: true); nothing to do"
        )
    url = str(_resolve_env_recursive(plane.url) or "").strip()
    workspace = str(_resolve_env_recursive(plane.workspace) or "").strip()
    token = str(_resolve_env_recursive(plane.token) or "").strip()
    if not url or not workspace:
        raise PlanePublishError(
            "integrations.plane must resolve non-empty 'url' and "
            "'workspace' values to publish pages."
        )
    if not token:
        ref = str(plane.token or "")
        if ref.startswith("${") and ref.endswith("}"):
            detail = f"the {ref[2:-1]!r} environment variable is unset or empty"
        else:
            detail = "no token is configured in the 'plane:' block"
        raise PlanePublishError(
            f"Plane credentials are not available: {detail}. Set it in "
            "your environment (or .env) before publishing pages."
        )
    return PlaneClient(url, token, workspace)


# ---------------------------------------------------------------------------
# Per-project page index
# ---------------------------------------------------------------------------


class _ProjectIndex:
    """One target project's page enumeration, indexed for idempotency.

    Built from a single narrow ``list_pages`` walk (:data:`_INDEX_FIELDS`).
    ``by_external`` holds only crewlet-marked pages keyed by their
    ``external_id`` — the primary, rename-stable match key *and* the
    prune candidate set.  ``by_name`` holds every page by exact name
    (first row wins) for the exact-title fallback and frontmatter
    ``parent`` resolution.
    """

    def __init__(self, identifier: str, uuid: str, pages: list[dict[str, Any]]) -> None:
        self.identifier = identifier
        self.uuid = uuid
        self.by_external: dict[str, dict[str, Any]] = {}
        self.by_name: dict[str, dict[str, Any]] = {}
        for page in pages:
            self.add(page)

    def add(self, page: dict[str, Any]) -> None:
        """Index one page — enumeration rows AND pages created mid-run.

        The publishers call this after every successful ``create_page``
        so the index stays consistent with what the run just published:
        a frontmatter ``parent`` naming a page published EARLIER IN THE
        SAME RUN resolves (instead of falling back to the project root
        with a hint that could never come true — the child would exist
        on the next run and existing pages are never re-parented), and
        ``--prune``'s candidate view matches reality at zero extra
        requests.
        """
        if not isinstance(page, dict) or not page.get("id"):
            return
        if _is_crewlet_page(page):
            self.by_external.setdefault(str(page.get("external_id") or ""), page)
        name = str(page.get("name") or "")
        if name:
            self.by_name.setdefault(name, page)

    def find(self, external_id: str, title: str) -> dict[str, Any] | None:
        """Match a page by external identity, falling back to exact title.

        The title fallback adopts only pages WITHOUT a crewlet external
        identity — a crewlet page carrying a *different* external id is
        a different record and must never be matched by its title.
        """
        page = self.by_external.get(external_id)
        if page is not None:
            return page
        page = self.by_name.get(title)
        if page is not None and not _is_crewlet_page(page):
            return page
        return None


def _is_crewlet_page(page: dict[str, Any]) -> bool:
    """Does this page carry the engine's external-identity marker?"""
    return str(page.get("external_source") or "") == EXTERNAL_SOURCE and bool(
        str(page.get("external_id") or "")
    )


# ---------------------------------------------------------------------------
# Per-file publishers
# ---------------------------------------------------------------------------


async def _import_skill(
    client: PlaneClient,
    project: _ProjectIndex,
    md_path: Path,
    text: str,
    *,
    update: bool,
    dry_run: bool,
) -> bool:
    """Publish one Tool Skill page into the skills project.

    Returns ``False`` when the page WRITE failed (isolated per page —
    the run continues and exits non-zero listing the failures); every
    other outcome (published, skipped, 409 conflict) returns ``True``.
    """
    frontmatter, body = parse_skill_file(text)
    skill_key = str(frontmatter.get("key", "")).strip()
    title = str(frontmatter.get("title", md_path.stem)).strip()
    if not skill_key:
        logger.warning(
            "skill_import_skipped",
            reason="missing_key",
            source=md_path.name,
        )
        return True

    # Stamp the effective enforcement state into the page's YAML block —
    # same rationale as the Confluence importer: the page is the
    # operator's editing surface, and the engine treats a synced skill
    # as enforced by default (SKILL_REQUIRED_DEFAULT).
    frontmatter.setdefault("required", SKILL_REQUIRED_DEFAULT)
    description_html = encode_page(frontmatter, body)
    external_id = skill_external_id(skill_key)
    existing = project.find(external_id, title)
    if existing is None:
        if dry_run:
            logger.info(
                "skill_imported",
                action="would_create",
                key=skill_key,
                project=project.identifier,
                source=md_path.name,
            )
            return True
        try:
            page = await client.create_page(
                project.uuid,
                name=title,
                description_html=description_html,
                external_id=external_id,
                external_source=EXTERNAL_SOURCE,
            )
        except PlaneProvisionError as exc:
            if exc.status == 409:
                _log_conflict(external_id, md_path, exc)
                return True
            return _handle_write_failure(md_path, exc)
        # Same-run visibility: later files can resolve this page as a
        # frontmatter ``parent``, and prune's view stays consistent.
        project.add(page)
        logger.info(
            "skill_imported",
            action="created",
            key=skill_key,
            page_id=str(page.get("id", "")),
            project=project.identifier,
            source=md_path.name,
        )
        return True
    page_id = str(existing.get("id", ""))
    if not update:
        logger.info(
            "skill_imported",
            action="skipped",
            key=skill_key,
            page_id=page_id,
            source=md_path.name,
            hint="pass --update to overwrite",
        )
        return True
    if dry_run:
        logger.info(
            "skill_imported",
            action="would_update",
            key=skill_key,
            page_id=page_id,
            source=md_path.name,
        )
        return True
    try:
        # The external pair rides along on every update: it self-heals
        # unmarked pages adopted via the title fallback, and the fork
        # skips the uniqueness lock when the identity is unchanged.
        await client.update_page(
            project.uuid,
            page_id,
            name=title,
            description_html=description_html,
            external_id=external_id,
            external_source=EXTERNAL_SOURCE,
        )
    except PlaneProvisionError as exc:
        if exc.status == 409:
            _log_conflict(external_id, md_path, exc)
            return True
        return _handle_write_failure(md_path, exc)
    logger.info(
        "skill_imported",
        action="updated",
        key=skill_key,
        page_id=page_id,
        project=project.identifier,
        source=md_path.name,
    )
    return True


async def _import_doc(
    client: PlaneClient,
    project: _ProjectIndex,
    md_path: Path,
    text: str,
    *,
    update: bool,
    dry_run: bool,
    warned_once: set[str],
) -> bool:
    """Publish one knowledge doc as clean prose into its target project.

    A frontmatter ``parent`` (a page title) nests a **newly created**
    page under that parent, resolved by exact name from the project's
    index — which includes pages published earlier in the same run; a
    missing parent falls back to the project root with a hint log.  An
    *existing* page is never re-parented — operators own the position
    of pages already in the project.

    Returns ``False`` when the page WRITE failed (isolated per page —
    the run continues and exits non-zero listing the failures); every
    other outcome returns ``True``.
    """
    try:
        frontmatter, title, body = parse_doc_file(text)
    except DocParseError as exc:
        logger.warning(
            "plane_import_skipped",
            reason="invalid_doc",
            source=md_path.name,
            error=str(exc),
        )
        return True
    if frontmatter.get("labels") and "labels" not in warned_once:
        warned_once.add("labels")
        logger.info(
            "plane_import_labels_ignored",
            source=md_path.name,
            hint="Plane pages carry no free-form labels; the engine's page "
            "marker is the external_id/external_source pair",
        )
    description_html = encode_doc(body)
    external_id = doc_external_id(title)
    existing = project.find(external_id, title)
    if existing is None:
        parent_id = _resolve_doc_parent(
            project, frontmatter, title=title, source=md_path.name
        )
        if dry_run:
            logger.info(
                "doc_imported",
                action="would_create",
                title=title,
                project=project.identifier,
                source=md_path.name,
            )
            return True
        try:
            page = await client.create_page(
                project.uuid,
                name=title,
                description_html=description_html,
                parent_id=parent_id,
                external_id=external_id,
                external_source=EXTERNAL_SOURCE,
            )
        except PlaneProvisionError as exc:
            if exc.status == 409:
                _log_conflict(external_id, md_path, exc)
                return True
            return _handle_write_failure(md_path, exc)
        # Same-run visibility: a doc published later in this run can
        # name this one as its frontmatter ``parent``.
        project.add(page)
        logger.info(
            "doc_imported",
            action="created",
            title=title,
            project=project.identifier,
            page_id=str(page.get("id", "")),
            source=md_path.name,
        )
        return True
    page_id = str(existing.get("id", ""))
    if not update:
        logger.info(
            "doc_imported",
            action="skipped",
            title=title,
            project=project.identifier,
            page_id=page_id,
            source=md_path.name,
            hint="pass --update to overwrite",
        )
        return True
    if dry_run:
        logger.info(
            "doc_imported",
            action="would_update",
            title=title,
            project=project.identifier,
            page_id=page_id,
            source=md_path.name,
        )
        return True
    try:
        await client.update_page(
            project.uuid,
            page_id,
            name=title,
            description_html=description_html,
            external_id=external_id,
            external_source=EXTERNAL_SOURCE,
        )
    except PlaneProvisionError as exc:
        if exc.status == 409:
            _log_conflict(external_id, md_path, exc)
            return True
        return _handle_write_failure(md_path, exc)
    logger.info(
        "doc_imported",
        action="updated",
        title=title,
        project=project.identifier,
        page_id=page_id,
        source=md_path.name,
    )
    return True


def _resolve_doc_parent(
    project: _ProjectIndex,
    frontmatter: dict[str, Any],
    *,
    title: str,
    source: str,
) -> str | None:
    """Resolve a doc's optional frontmatter ``parent`` to a page id.

    ``parent`` names an existing page by exact name in the doc's own
    target project, matched against the project index — the initial
    enumeration PLUS every page this run already created (see
    :meth:`_ProjectIndex.add`), so a parent published earlier in the
    same run resolves with no extra request.  Returns ``None`` when no
    parent is declared or the named page doesn't exist — the latter
    logs a hint and the doc lands at the project root rather than
    failing the import.
    """
    parent_title = str(frontmatter.get("parent", "") or "").strip()
    if not parent_title:
        return None
    parent_page = project.by_name.get(parent_title)
    if parent_page is not None:
        return str(parent_page.get("id", "")) or None
    logger.info(
        "doc_parent_not_found",
        parent=parent_title,
        project=project.identifier,
        title=title,
        source=source,
        hint="creating at the project root; create the parent page in "
        "Plane (or import it — sorted before this file — in the same "
        "run) and re-parent this page manually, since existing pages "
        "are never re-parented",
    )
    return None


def _handle_write_failure(md_path: Path, exc: PlaneProvisionError) -> bool:
    """Per-page isolation for a non-409 create/update failure.

    One page's failure must not abort the run — the same contract the
    409 path and prune already honour: a single LOCKED page (a normal
    Plane UI gesture on a reviewed skill page — the fork PATCHes it
    with ``400 "Page is locked"``) or a page-level 403 must not leave
    every later file unpublished.  Logs ``plane_page_write_failed``
    with a status-specific remediation and returns ``False`` so the
    run records the page and exits non-zero.

    The one exception is 401: an invalid credential fails every
    subsequent request identically, so it re-raises into the run-level
    abort (which carries the credential hint).
    """
    if exc.status == 401:
        raise exc
    if exc.status == 400 and "locked" in str(exc).lower():
        hint = "the page is locked in Plane; unlock it, then re-run"
    elif exc.status == 403:
        hint = (
            "the importing account lacks permission on this page; grant "
            "it access (or have the owner update the page), then re-run"
        )
    else:
        hint = "fix the page in Plane, then re-run"
    logger.warning(
        "plane_page_write_failed",
        source=md_path.name,
        status=exc.status,
        error=str(exc),
        hint=hint,
    )
    return False


def _log_conflict(external_id: str, md_path: Path, exc: PlaneProvisionError) -> None:
    """Log a 409 external-identity conflict and let the run continue.

    The fork enforces project-wide uniqueness of the
    ``external_id``/``external_source`` pair (including on ARCHIVED
    pages, which the importer's default enumeration doesn't see) — so a
    409 means another page already owns this identity.  One page's
    conflict must not abort the whole publish run.
    """
    logger.warning(
        "plane_page_conflict",
        external_id=external_id,
        source=md_path.name,
        error=str(exc),
        hint="another page (possibly archived) already carries this "
        "external identity; unarchive or delete it in Plane, then re-run",
    )


# ---------------------------------------------------------------------------
# Prune
# ---------------------------------------------------------------------------


async def _prune_orphan_skills(
    client: PlaneClient,
    project: _ProjectIndex,
    local_keys: set[str],
    *,
    dry_run: bool,
) -> None:
    """Delete import-managed skill pages whose source ``.md`` was removed.

    Positive-marker predicate: a page is an orphan ONLY when it carries
    ``external_source == "crewlet"`` AND a ``skill:`` external id whose
    key no local file publishes.  Unmarked (user-authored) pages,
    ``doc:``-keyed pages, and knowledge docs are structurally out of
    reach.  Deletion honors the fork's archive-then-delete precondition
    (a live page 400s on DELETE) with per-page failure isolation — a
    failure on one page logs and continues, never aborts the run — and
    the two steps fail independently:

    * **archive failed** → the page is untouched; ``skill_prune_failed``.
    * **delete failed** (owner-or-project-admin only, so a 403 on a
      human-owned page is the documented case) → the archive is UNDONE
      (best-effort ``unarchive_page``) so a failed prune is a true
      no-op.  Leaving the page archived would hide it from users and
      from every later enumeration while its ``external_id`` — which
      the fork's uniqueness check matches regardless of archived state
      — 409s every future republish of the same identity: a permanent
      conflict only a human unarchive could clear.  Logged as
      ``plane_prune_delete_failed`` with the resulting visibility.

    The candidate set is the import's own enumeration index; when that
    enumeration failed, the whole run aborted before reaching here — an
    incomplete listing must delete nothing.
    """
    local_ids = {skill_external_id(key) for key in local_keys}
    for external_id, page in sorted(project.by_external.items()):
        if not external_id.startswith(_SKILL_EXTERNAL_PREFIX):
            continue
        if external_id in local_ids:
            continue
        page_id = str(page.get("id", ""))
        title = str(page.get("name", ""))
        if dry_run:
            logger.info(
                "skill_pruned", action="would_delete", page_id=page_id, title=title
            )
            continue
        try:
            await client.archive_page(project.uuid, page_id)
        except PlaneProvisionError as exc:
            logger.warning(
                "skill_prune_failed",
                page_id=page_id,
                title=title,
                stage="archive",
                status=exc.status,
                error=str(exc),
            )
            continue
        try:
            await client.delete_page(project.uuid, page_id)
        except PlaneProvisionError as exc:
            unarchived = True
            try:
                await client.unarchive_page(project.uuid, page_id)
            except PlaneProvisionError as unarchive_exc:
                unarchived = False
                logger.warning(
                    "skill_prune_unarchive_failed",
                    page_id=page_id,
                    title=title,
                    status=unarchive_exc.status,
                    error=str(unarchive_exc),
                )
            logger.warning(
                "plane_prune_delete_failed",
                page_id=page_id,
                title=title,
                status=exc.status,
                error=str(exc),
                unarchived=unarchived,
                hint=(
                    "the page was left visible (archive rolled back)"
                    if unarchived
                    else "the page was left ARCHIVED — unarchive it in Plane "
                    "or its external_id will 409 every future republish"
                ),
            )
            continue
        logger.info("skill_pruned", action="deleted", page_id=page_id, title=title)


# ---------------------------------------------------------------------------
# Shared async core
# ---------------------------------------------------------------------------


async def run_import(
    *,
    config_path: Path,
    paths: list[Path],
    update: bool = False,
    dry_run: bool = False,
    prune: bool = False,
    skills_project: str,
) -> int:
    """Shared async core for the unified Plane import flow.

    Used by ``cmd_plane_import`` and by ``cmd_run`` (when invoked with
    ``--import-plane``) so the same code path seeds Plane whether the
    operator runs the import as a one-shot or bundles it with the
    engine start.

    Collects every ``*.md`` under ``paths`` (recursively), routes each
    by frontmatter, pre-flights every distinct target project (the
    importer never creates containers — project creation is the
    provisioner's job), builds ONE page-enumeration index per project,
    then publishes.  Per-page write failures (a locked page, a
    page-level 403) are isolated — the rest of the run still publishes
    — but the run then raises :class:`PlanePublishError` naming the
    failed files, so it exits non-zero.  Returns 0 only when every
    page succeeded; raises :class:`PlanePublishError` on any failure.
    """
    files = collect_md_files(paths)

    # Route every file once up front: the same read feeds the target
    # pre-flight, the local-key set for --prune, and the publish loop.
    routed: list[tuple[Path, str, str]] = []
    local_skill_keys: set[str] = set()
    have_skills = False
    for md_path in files:
        text = md_path.read_text()
        try:
            frontmatter, _body = split_optional_frontmatter(text)
        except DocParseError as exc:
            logger.warning(
                "plane_import_skipped",
                reason="invalid_frontmatter",
                source=md_path.name,
                error=str(exc),
            )
            continue
        kind = classify(frontmatter)
        routed.append((md_path, kind, text))
        if kind == "skill":
            have_skills = True
            key = str(frontmatter.get("key", "")).strip()
            if key:
                local_skill_keys.add(key)

    targets: set[str] = set()
    if have_skills or prune:
        if not skills_project:
            raise PlanePublishError(
                "no Tool Skills project is configured: pass --project or "
                "set CREWLET_TOOL_SKILLS_PROJECT to the skills project "
                "identifier."
            )
        targets.add(skills_project)
    targets.update(
        md_path.parent.name for md_path, kind, _text in routed if kind == "doc"
    )

    client = await build_client_from_config(config_path)
    try:
        try:
            # Pre-flight: resolve every distinct target identifier
            # (case-insensitive) against the workspace's projects and
            # fail fast, with remediation, before any page is written.
            rows = await client.list_projects()
            ident_to_uuid: dict[str, str] = {}
            for row in rows:
                ident = str(row.get("identifier") or "").strip().upper()
                uuid = str(row.get("id") or "")
                if ident and uuid:
                    ident_to_uuid.setdefault(ident, uuid)
            indexes: dict[str, _ProjectIndex] = {}
            missing: list[str] = []
            for target in sorted(targets):
                uuid = ident_to_uuid.get(target.strip().upper(), "")
                if not uuid:
                    missing.append(target)
            if missing:
                raise PlanePublishError(
                    f"Plane project(s) not found: {', '.join(missing)}. "
                    f"Create each project in Plane (any name, identifier "
                    f"set to the value above), or provision it via "
                    f"`crewlet plane provision` (provisioning.projects) "
                    f"once slice 4 lands. Skill files target --project / "
                    f"CREWLET_TOOL_SKILLS_PROJECT; knowledge docs target "
                    f"their parent directory name."
                )

            # ONE narrow enumeration per target project, indexed by
            # external identity (idempotency) and exact name (unmarked
            # title fallback + frontmatter ``parent`` resolution).
            for target in sorted(targets):
                key = target.strip().upper()
                if key in indexes:
                    continue
                uuid = ident_to_uuid[key]
                pages = await client.list_pages(uuid, fields=_INDEX_FIELDS)
                indexes[key] = _ProjectIndex(target, uuid, pages)

            logger.info(
                "plane_import_started",
                count=len(files),
                projects=sorted(targets),
                update=update,
                dry_run=dry_run,
            )
            warned_once: set[str] = set()
            failed_pages: list[str] = []
            for md_path, kind, text in routed:
                if kind == "skill":
                    ok = await _import_skill(
                        client,
                        indexes[skills_project.strip().upper()],
                        md_path,
                        text,
                        update=update,
                        dry_run=dry_run,
                    )
                else:
                    ok = await _import_doc(
                        client,
                        indexes[md_path.parent.name.strip().upper()],
                        md_path,
                        text,
                        update=update,
                        dry_run=dry_run,
                        warned_once=warned_once,
                    )
                if not ok:
                    failed_pages.append(md_path.name)
            if prune:
                await _prune_orphan_skills(
                    client,
                    indexes[skills_project.strip().upper()],
                    local_skill_keys,
                    dry_run=dry_run,
                )
            if failed_pages:
                # Every page got its chance (per-page isolation), but a
                # partially published run must not exit 0 — CI and
                # ``crewlet run --import-plane`` need to see it.
                raise PlanePublishError(
                    f"{len(failed_pages)} page(s) failed to publish: "
                    f"{', '.join(sorted(failed_pages))}. The rest of the "
                    f"run completed; see the plane_page_write_failed log "
                    f"lines for the per-page remediation, then re-run."
                )
        except PlaneProvisionError as exc:
            # Surface a friendly message instead of a raw stack trace
            # for any Plane failure (auth, permissions, connectivity).
            hint = ""
            if exc.status in (401, 403):
                hint = (
                    " — check the Plane token in the company config's "
                    "'integrations.plane' block and that the account is a "
                    "member of the target project(s)"
                )
            raise PlanePublishError(
                f"Plane API error during import: {exc}{hint}"
            ) from exc
        logger.info(
            "plane_import_completed",
            count=len(files),
            projects=sorted(targets),
        )
    finally:
        await client.close()
    return 0


# ---------------------------------------------------------------------------
# Import command
# ---------------------------------------------------------------------------


def cmd_plane_import(args: Any) -> int:
    """Publish local skill + knowledge-doc markdown into Plane."""
    config_path: Path = args.config
    # Honour a project .env the same way ``crewlet run`` does so the
    # Plane credentials referenced by the company YAML resolve from the
    # environment even when they live only in .env.
    load_env_file(config_path)
    if not config_path.exists():
        logger.error("plane_import_config_missing", path=str(config_path))
        return 1
    path: Path = args.path
    if not path.exists():
        logger.error("plane_import_path_missing", path=str(path))
        return 1
    skills_project = _resolve_skills_project(args.project)

    try:
        return asyncio.run(
            run_import(
                config_path=config_path,
                paths=[path],
                update=bool(args.update),
                dry_run=bool(args.dry_run),
                prune=bool(getattr(args, "prune", False)),
                skills_project=skills_project,
            )
        )
    except PlanePublishError as exc:
        logger.error("plane_import_failed", error=str(exc))
        return 1
    except (RuntimeError, ValueError) as exc:
        logger.error("plane_import_failed", error=str(exc))
        return 1


# ---------------------------------------------------------------------------
# Resync command (skills registry only)
# ---------------------------------------------------------------------------


async def sync_skills_registry(
    client: PlaneClient,
    registry: PromptSkillRegistry,
    skills_project: str,
) -> int:
    """Full populate of a skills registry from the Tool Skills project.

    ONE strict enumeration carrying ``description_html`` (``fields=``),
    then :func:`~crewlet.agent.skills.plane_sync.page_to_skill` — the
    exact decode + admission logic the engine's boot walk runs — for
    each row, and an atomic registry seed.  A missing project logs and
    loads nothing.  Returns the loaded count.
    """
    from crewlet.agent.skills.plane_sync import SYNC_PAGE_FIELDS, page_to_skill

    rows = await client.list_projects()
    uuid = ""
    for row in rows:
        ident = str(row.get("identifier") or "").strip().upper()
        if ident and ident == skills_project.strip().upper():
            uuid = str(row.get("id") or "")
            break
    if not uuid:
        logger.warning("tool_skill_project_not_found", project=skills_project)
        return 0
    pages = await client.list_pages(uuid, fields=SYNC_PAGE_FIELDS)
    loaded = [
        skill
        for page in pages
        if isinstance(page, dict) and (skill := page_to_skill(page)) is not None
    ]
    registry.seed(loaded)
    return len(loaded)


def cmd_plane_resync(args: Any) -> int:
    """Re-fetch the Tool Skills project and dump what would be loaded.

    This is a diagnostic / drift-recovery tool. It uses a *temporary*
    PromptSkillRegistry isolated from any running engine: a live engine
    receives Plane page webhooks directly (create / content-persist
    update / delete), so a manual resync is only needed when an operator
    suspects a webhook was missed across a long outage. The CLI re-runs
    the same full-populate logic and reports counts; restart the running
    engine (or wait for the next webhook) to apply changes there.

    Resync is skills-registry-only — knowledge docs are searched live
    and are never loaded into a registry, so there is nothing to resync
    for them.
    """
    config_path: Path = args.config
    load_env_file(config_path)
    if not config_path.exists():
        logger.error("skill_resync_config_missing", path=str(config_path))
        return 1
    skills_project = _resolve_skills_project(args.project)

    async def _run() -> int:
        client = await build_client_from_config(config_path)
        registry = PromptSkillRegistry()
        try:
            count = await sync_skills_registry(client, registry, skills_project)
        finally:
            await client.close()
        logger.info(
            "skill_resync_completed",
            project=skills_project,
            count=count,
            keys=registry.keys(),
        )
        return 0

    try:
        return asyncio.run(_run())
    except PlanePublishError as exc:
        logger.error("skill_resync_failed", error=str(exc))
        return 1
    except PlaneProvisionError as exc:
        logger.error("skill_resync_failed", error=str(exc))
        return 1
    except (RuntimeError, ValueError) as exc:
        logger.error("skill_resync_failed", error=str(exc))
        return 1


__all__ = [
    "EXTERNAL_SOURCE",
    "PlanePublishError",
    "build_client_from_config",
    "cmd_plane_import",
    "cmd_plane_resync",
    "doc_external_id",
    "run_import",
    "skill_external_id",
    "sync_skills_registry",
]
