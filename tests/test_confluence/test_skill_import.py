"""Tests for ``crewlet confluence import`` — tool-skill path.

Drive the unified importer's skill branch against an httpx.MockTransport,
patching out the ConfluenceTransport build step. Each test exercises one
operator-facing behavior — create new pages, skip existing, update when
``--update``, dry-run when ``--dry-run``, etc. The skill wire format
(YAML-macro encoding, two labels, required stamp) is pinned
byte-for-byte.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import httpx
import pytest

from crewlet.confluence import import_cli, pages
from crewlet.confluence.import_cli import _skill_key_label, import_one
from crewlet.confluence.pages import (
    ConfluencePublishError,
    check_space_exists,
    create_space,
    find_by_title,
    safe_label,
)

EXAMPLES_DIR = (
    Path(__file__).resolve().parent.parent.parent / "examples" / "tool-skills"
)


class _FakeTransport:
    """Stand-in for ConfluenceTransport with the httpx + REST surface
    the publish helpers consume.  Records every outgoing request so tests
    can assert on payload / verb / URL."""

    def __init__(self, handler: httpx.MockTransport) -> None:
        self._http_client = httpx.AsyncClient(transport=handler)
        self._rest_base = "https://confluence.example.com/wiki"
        self.requests: list[httpx.Request] = []

    async def stop(self) -> None:
        await self._http_client.aclose()


def test_skill_key_label_uses_only_confluence_safe_characters() -> None:
    """Confluence labels reject anything outside ``[a-z0-9_-]`` with a
    400 ``label.contains.invalid.chars``. Verify every chunk of every
    bundled skill's encoded label survives that filter."""
    import re

    safe_re = re.compile(r"^[a-z0-9_-]+$")
    for key in (
        "mcp:github",
        "tool:reflect_and_persist",
        "skill:platform_mentions",
        "skill:retrieval_research",
        "tool:refine_skill",
        "skill:observed_directives",
    ):
        label = _skill_key_label(key)
        assert safe_re.match(label), (
            f"label {label!r} for skill {key!r} contains invalid chars"
        )
        # Encoding is lossy (':' → '_') but must be deterministic so
        # the same key always produces the same label.
        assert _skill_key_label(key) == label


def test_skill_key_label_lowercases_and_replaces_uppercase_and_punctuation() -> None:
    """Operators authoring custom skills could use uppercase or
    punctuation; the encoder normalises to the label-safe form rather
    than silently producing a label Confluence will reject."""
    assert _skill_key_label("MCP:GitHub") == "crewlet-skill-key-mcp_github"
    assert (
        _skill_key_label("skill:weird.name!") == "crewlet-skill-key-skill_weird_name_"
    )


def test_safe_label_generalizes_prefix() -> None:
    """``safe_label`` is the shared generic; the skill key label is just
    one prefix over it."""
    assert safe_label("mcp:github", prefix="crewlet-skill-key") == (
        "crewlet-skill-key-mcp_github"
    )
    assert safe_label("LEAD:Manager 1:1", prefix="crewlet-doc-key") == (
        "crewlet-doc-key-lead_manager_1_1"
    )


@pytest.mark.asyncio
async def test_find_by_title_returns_none_when_no_match() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"results": []})

    fake = _FakeTransport(httpx.MockTransport(handler))
    assert await find_by_title(fake, "TS", "Some title that doesn't exist") is None
    await fake.stop()


@pytest.mark.asyncio
async def test_import_one_update_path_attaches_labels() -> None:
    """When --update finds the page via title fallback (label missing),
    the update path re-attaches the label so the next run finds it
    via the fast label-CQL lookup."""
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"
    put_calls = 0
    label_payloads: list[Any] = []

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal put_calls
        # Label CQL search returns no hits → forces title fallback.
        if request.method == "GET" and "search" in request.url.path:
            return httpx.Response(200, json={"results": []})
        # Title-based fallback returns the existing page.
        if request.method == "GET" and "title=" in request.url.query.decode():
            return httpx.Response(
                200,
                json={"results": [{"id": "77", "version": {"number": 3}}]},
            )
        if request.method == "POST" and request.url.path.endswith("/label"):
            label_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"results": []})
        if request.method == "PUT":
            put_calls += 1
            return httpx.Response(200, json={"id": "77"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, skill_path, update=True, dry_run=False, skill_space="TS")
    assert put_calls == 1
    # The update path re-attached labels.
    assert len(label_payloads) == 1
    label_names = [entry["name"] for entry in label_payloads[0]]
    assert "crewlet-skill" in label_names
    assert any(name.startswith("crewlet-skill-key-") for name in label_names)
    await fake.stop()


@pytest.mark.asyncio
async def test_import_one_creates_when_page_missing(tmp_path: Path) -> None:
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"
    create_payloads: list[dict[str, Any]] = []
    label_payloads: list[Any] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":  # search-by-label
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            label_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":  # create
            create_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "999"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, skill_path, update=False, dry_run=False, skill_space="TS")
    # Create payload is now label-free; Cloud's v1 parser is finicky
    # about ``metadata.labels`` on create, so we POST labels separately.
    assert len(create_payloads) == 1
    create_body = create_payloads[0]
    assert create_body["space"]["key"] == "TS"
    assert "metadata" not in create_body
    # Labels arrive on the follow-up POST to /content/{id}/label.
    assert len(label_payloads) == 1
    label_names = [entry["name"] for entry in label_payloads[0]]
    assert "crewlet-skill" in label_names
    assert any(name.startswith("crewlet-skill-key-") for name in label_names)
    await fake.stop()


@pytest.mark.asyncio
async def test_import_one_stamps_required_default_into_page_yaml() -> None:
    """A source file that omits ``required:`` must still produce a page
    whose YAML box shows the effective enforcement state — the page is
    the operator's editing surface, and the engine treats the synced
    skill as enforced (SKILL_REQUIRED_DEFAULT). Without the stamp the
    page would silently hide that the skill gates tool calls."""
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"
    assert "required" not in skill_path.read_text().split("---")[1]
    create_payloads: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            create_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "999"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, skill_path, update=False, dry_run=False, skill_space="TS")
    assert len(create_payloads) == 1
    storage = create_payloads[0]["body"]["storage"]["value"]
    assert "required: true" in storage


@pytest.mark.asyncio
async def test_import_one_skips_existing_without_update_flag() -> None:
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"
    posts_or_puts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts_or_puts
        if request.method == "GET":
            return httpx.Response(
                200,
                json={
                    "results": [{"id": "42", "version": {"number": 1}}],
                },
            )
        posts_or_puts += 1
        return httpx.Response(200, json={"id": "42"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, skill_path, update=False, dry_run=False, skill_space="TS")
    assert posts_or_puts == 0  # nothing was created or updated
    await fake.stop()


@pytest.mark.asyncio
async def test_import_one_updates_existing_when_update_flag_set() -> None:
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"
    put_payloads: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(
                200,
                json={
                    "results": [{"id": "42", "version": {"number": 1}}],
                },
            )
        if request.method == "PUT":
            put_payloads.append(json.loads(request.content))
            return httpx.Response(200, json={"id": "42"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, skill_path, update=True, dry_run=False, skill_space="TS")
    assert len(put_payloads) == 1
    assert put_payloads[0]["version"]["number"] == 2  # bumped from 1
    await fake.stop()


@pytest.mark.asyncio
async def test_import_one_dry_run_makes_no_post_or_put() -> None:
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"
    posts_or_puts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts_or_puts
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        posts_or_puts += 1
        return httpx.Response(200, json={"id": "999"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    await import_one(fake, skill_path, update=False, dry_run=True, skill_space="TS")
    assert posts_or_puts == 0
    await fake.stop()


@pytest.mark.asyncio
async def test_import_one_skips_malformed_skill_file(tmp_path: Path) -> None:
    bad = tmp_path / "bad.md"
    bad.write_text("no frontmatter here\n")
    posts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts
        if request.method == "POST":
            posts += 1
        return httpx.Response(200, json={"results": []})

    fake = _FakeTransport(httpx.MockTransport(handler))
    # Malformed file → parser failure → logged as confluence_import_skipped
    # and no Confluence calls are made.
    await import_one(fake, bad, update=False, dry_run=False, skill_space="TS")
    assert posts == 0
    await fake.stop()


def test_cmd_confluence_import_errors_when_config_missing(tmp_path: Path) -> None:
    args = SimpleNamespace(
        config=tmp_path / "nope.yaml",
        path=EXAMPLES_DIR,
        space="TS",
        update=False,
        dry_run=False,
        create_space=False,
    )
    rc = import_cli.cmd_confluence_import(args)
    assert rc == 1


def test_cmd_confluence_import_errors_when_path_missing(tmp_path: Path) -> None:
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("name: x\n")
    args = SimpleNamespace(
        config=cfg,
        path=tmp_path / "no-such-dir",
        space="TS",
        update=False,
        dry_run=False,
        create_space=False,
    )
    rc = import_cli.cmd_confluence_import(args)
    assert rc == 1


def test_cmd_confluence_import_loads_dotenv_beside_config(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """The standalone import honours a project ``.env`` (like ``crewlet
    run``) so Confluence credentials kept only in ``.env`` resolve from
    the environment — matching the credential-error hint it prints."""
    monkeypatch.delenv("CREWLET_DOTENV_PROBE", raising=False)
    cfg = tmp_path / "company.yaml"
    cfg.write_text("name: x\n")
    (tmp_path / ".env").write_text("CREWLET_DOTENV_PROBE=loaded\n")
    docs = tmp_path / "docs"
    docs.mkdir()
    (docs / "a.md").write_text("# A\n\nbody\n")

    seen: dict[str, str | None] = {}

    async def fake_run_import(**_kw: Any) -> int:
        seen["probe"] = os.environ.get("CREWLET_DOTENV_PROBE")
        return 0

    monkeypatch.setattr(import_cli, "run_import", fake_run_import)
    args = SimpleNamespace(
        config=cfg,
        path=docs,
        space="TS",
        update=True,
        dry_run=False,
        create_space=False,
    )
    rc = import_cli.cmd_confluence_import(args)
    os.environ.pop("CREWLET_DOTENV_PROBE", None)
    assert rc == 0
    assert seen["probe"] == "loaded"


@pytest.mark.asyncio
async def test_run_import_shared_core_processes_each_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """``run_import`` is the shared async core used by both
    ``crewlet confluence import`` and ``crewlet run --import-confluence``.
    Patch out the transport build so we don't need a real config."""
    create_calls = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal create_calls
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            create_calls += 1
            return httpx.Response(200, json={"id": str(1000 + create_calls)})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    n_files = len(sorted(EXAMPLES_DIR.glob("*.md")))
    rc = await import_cli.run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[EXAMPLES_DIR],
        update=False,
        dry_run=False,
        skill_space="TS",
    )
    assert rc == 0
    assert create_calls == n_files


@pytest.mark.asyncio
async def test_check_space_exists_reports_true_on_200() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith("/rest/api/space/TS")
        return httpx.Response(200, json={"key": "TS"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    assert await check_space_exists(fake, "TS") is True
    await fake.stop()


@pytest.mark.asyncio
async def test_check_space_exists_reports_false_on_404() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404, json={"message": "no space"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    assert await check_space_exists(fake, "TS") is False
    await fake.stop()


@pytest.mark.asyncio
async def test_check_space_exists_returns_none_on_other_status() -> None:
    """403 / 503 / network errors → caller falls through to per-page
    calls, which surface the real error in context."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(403, json={"message": "forbidden"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    assert await check_space_exists(fake, "TS") is None
    await fake.stop()


@pytest.mark.asyncio
async def test_run_import_raises_publish_error_when_space_missing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Pre-flight space check → if 404, surface a friendly error
    *before* the per-page loop tries (and fails) to create pages."""
    posts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal posts
        if request.method == "GET" and "/space/" in request.url.path:
            return httpx.Response(404, json={"message": "no space"})
        if request.method == "POST":
            posts += 1
        return httpx.Response(200, json={"results": []})

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    with pytest.raises(ConfluencePublishError, match="not found"):
        await import_cli.run_import(
            config_path=tmp_path / "cfg.yaml",
            paths=[EXAMPLES_DIR],
            update=False,
            dry_run=False,
            skill_space="TS",
        )
    # No page POST should have happened — pre-flight aborted early.
    assert posts == 0


@pytest.mark.asyncio
async def test_create_page_raises_publish_error_on_404_from_post() -> None:
    """If the pre-flight didn't catch it (e.g. race or unusual error),
    a 404 from the POST itself also surfaces a friendly message."""
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            return httpx.Response(404, json={"message": "no space"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    with pytest.raises(ConfluencePublishError, match="404"):
        await import_one(
            fake, skill_path, update=False, dry_run=False, skill_space="TS"
        )
    await fake.stop()


@pytest.mark.asyncio
async def test_create_page_surfaces_confluence_400_message() -> None:
    """A bare 400 from Confluence is useless to the operator; we need
    to surface the response body so the actual rejection reason is
    visible."""
    skill_path = EXAMPLES_DIR / "reflect-and-persist.md"

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            return httpx.Response(400, text="ParseException: invalid storage format")
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    with pytest.raises(ConfluencePublishError, match="ParseException"):
        await import_one(
            fake, skill_path, update=False, dry_run=False, skill_space="TS"
        )
    await fake.stop()


@pytest.mark.asyncio
async def test_create_space_posts_to_v1_space_endpoint() -> None:
    captured: dict[str, Any] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        assert request.method == "POST"
        assert request.url.path.endswith("/rest/api/space")
        captured["body"] = json.loads(request.content)
        return httpx.Response(200, json={"key": "TS"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    await create_space(fake, "TS")
    assert captured["body"]["key"] == "TS"
    assert captured["body"]["name"]  # default applied
    assert captured["body"]["description"]["plain"]["value"]
    await fake.stop()


@pytest.mark.asyncio
async def test_create_space_surfaces_friendly_error_on_403() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(403, json={"message": "forbidden"})

    fake = _FakeTransport(httpx.MockTransport(handler))
    with pytest.raises(ConfluencePublishError, match="space-admin"):
        await create_space(fake, "TS")
    await fake.stop()


@pytest.mark.asyncio
async def test_run_import_auto_creates_space_when_flag_set(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Pre-flight 404 + --create-space → POST /rest/api/space, then
    proceed with the per-page upload normally."""
    space_post = 0
    page_posts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal space_post, page_posts
        if request.method == "GET" and "/space/" in request.url.path:
            return httpx.Response(404, json={"message": "no space"})
        if request.method == "POST" and request.url.path.endswith("/space"):
            space_post += 1
            return httpx.Response(200, json={"key": "TS"})
        if request.method == "GET" and "search" in request.url.path:
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and request.url.path.endswith("/label"):
            return httpx.Response(200, json={"results": []})
        if request.method == "POST" and "/content" in request.url.path:
            page_posts += 1
            return httpx.Response(200, json={"id": str(page_posts)})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    n_files = len(sorted(EXAMPLES_DIR.glob("*.md")))
    rc = await import_cli.run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[EXAMPLES_DIR],
        update=False,
        dry_run=False,
        create_space=True,
        skill_space="TS",
    )
    assert rc == 0
    assert space_post == 1
    assert page_posts == n_files


@pytest.mark.asyncio
async def test_run_import_dry_run_does_not_create_space_for_real(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """--dry-run + --create-space should report the would-create action
    without actually POSTing to the space endpoint."""
    space_posts = 0
    page_posts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal space_posts, page_posts
        if request.method == "GET" and "/space/" in request.url.path:
            return httpx.Response(404, json={"message": "no space"})
        if request.method == "POST" and request.url.path.endswith("/space"):
            space_posts += 1
            return httpx.Response(200, json={"key": "TS"})
        if request.method == "GET":
            return httpx.Response(200, json={"results": []})
        if request.method == "POST":
            page_posts += 1
            return httpx.Response(200, json={"id": "1"})
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    monkeypatch.setattr(pages, "build_transport", _fake_build)

    rc = await import_cli.run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[EXAMPLES_DIR],
        update=False,
        dry_run=True,
        create_space=True,
        skill_space="TS",
    )
    assert rc == 0
    assert space_posts == 0  # nothing actually created
    assert page_posts == 0  # dry-run: no page uploads either


_CONFLUENCE_COMPANY_YAML = (
    "name: T\n"
    "integrations:\n"
    "  confluence:\n"
    '    url: "https://x.atlassian.net/wiki"\n'
    '    token: "${JIRA_ADMIN_API_TOKEN}"\n'
    '    email: "${JIRA_ADMIN_EMAIL}"\n'
)


@pytest.mark.asyncio
async def test_build_transport_resolves_confluence_env_vars(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """``build_transport`` resolves ${VAR} confluence creds against the
    environment (load_company_config keeps them verbatim), so the
    transport authenticates with real values, not literal ``${...}``."""
    monkeypatch.setenv("JIRA_ADMIN_API_TOKEN", "secret-token")
    monkeypatch.setenv("JIRA_ADMIN_EMAIL", "admin@example.com")
    company = tmp_path / "company.yaml"
    company.write_text(_CONFLUENCE_COMPANY_YAML)

    captured: dict[str, Any] = {}

    class _FakeCT:
        def __init__(self, cfg: Any) -> None:
            captured["cfg"] = cfg

        async def start(self) -> None: ...

        async def stop(self) -> None: ...

    import crewlet.notifications.transports.confluence as ct_mod

    monkeypatch.setattr(ct_mod, "ConfluenceTransport", _FakeCT)

    await pages.build_transport(company)
    assert captured["cfg"].token == "secret-token"
    assert captured["cfg"].email == "admin@example.com"


@pytest.mark.asyncio
async def test_build_transport_errors_when_credentials_unset(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """Empty resolved creds fail fast with a message naming the env var,
    not an opaque empty-auth-header error on every request."""
    monkeypatch.delenv("JIRA_ADMIN_API_TOKEN", raising=False)
    monkeypatch.delenv("JIRA_ADMIN_EMAIL", raising=False)
    company = tmp_path / "company.yaml"
    company.write_text(_CONFLUENCE_COMPANY_YAML)
    with pytest.raises(ConfluencePublishError, match="JIRA_ADMIN_API_TOKEN"):
        await pages.build_transport(company)


@pytest.mark.asyncio
async def test_run_import_wraps_http_errors_as_publish_error(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """A Confluence HTTP failure during the import loop surfaces as a
    friendly ConfluencePublishError, not a raw httpx traceback."""
    fake = _FakeTransport(httpx.MockTransport(lambda _r: httpx.Response(200, json={})))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    async def _space_ok(transport: Any, space_key: str) -> bool:
        return True

    async def _boom(*_a: Any, **_k: Any) -> None:
        raise httpx.HTTPStatusError(
            "unauthorized",
            request=httpx.Request("POST", "https://x/rest/api/content"),
            response=httpx.Response(401),
        )

    monkeypatch.setattr(pages, "build_transport", _fake_build)
    monkeypatch.setattr(pages, "check_space_exists", _space_ok)
    monkeypatch.setattr(import_cli, "import_one", _boom)

    with pytest.raises(ConfluencePublishError, match="401"):
        await import_cli.run_import(
            config_path=tmp_path / "cfg.yaml",
            paths=sorted(EXAMPLES_DIR.glob("*.md"))[:1],
            update=False,
            dry_run=False,
            skill_space="TS",
        )


# ---------------------------------------------------------------------------
# --prune: orphaned skill-page cleanup
# ---------------------------------------------------------------------------


def _skill_page(page_id: str, key: str, title: str = "t") -> dict[str, Any]:
    from crewlet.agent.skills.labels import SKILL_MARKER_LABEL

    return {
        "id": page_id,
        "title": title,
        "metadata": {
            "labels": {
                "results": [
                    {"name": SKILL_MARKER_LABEL},
                    {"name": _skill_key_label(key)},
                ]
            }
        },
    }


@pytest.mark.asyncio
async def test_find_all_by_label_lists_with_labels_expanded() -> None:
    from crewlet.confluence.pages import find_all_by_label

    def handler(request: httpx.Request) -> httpx.Response:
        assert "metadata.labels" in request.url.query.decode()
        return httpx.Response(200, json={"results": [{"id": "1"}, {"id": "2"}]})

    fake = _FakeTransport(httpx.MockTransport(handler))
    out = await find_all_by_label(fake, "TS", "crewlet-skill")
    assert [p["id"] for p in out] == ["1", "2"]
    await fake.stop()


@pytest.mark.asyncio
async def test_delete_page_success_and_failure() -> None:
    from crewlet.confluence.pages import delete_page

    fake_ok = _FakeTransport(httpx.MockTransport(lambda _r: httpx.Response(204)))
    assert await delete_page(fake_ok, "5") is True
    await fake_ok.stop()

    fake_bad = _FakeTransport(httpx.MockTransport(lambda _r: httpx.Response(403)))
    assert await delete_page(fake_bad, "5") is False
    await fake_bad.stop()


@pytest.mark.asyncio
async def test_prune_deletes_only_orphan_skill_pages() -> None:
    """A skill page whose key is no longer local is deleted; a page whose
    key IS local is kept."""
    from crewlet.confluence.import_cli import _prune_orphan_skills

    deleted: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET" and "search" in request.url.path:
            return httpx.Response(
                200,
                json={
                    "results": [
                        _skill_page("1", "mcp:github", "GitHub tools"),
                        _skill_page("2", "skill:copilot_delegation", "Copilot"),
                    ]
                },
            )
        if request.method == "DELETE":
            deleted.append(request.url.path.rsplit("/", 1)[-1])
            return httpx.Response(204)
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    # mcp:github is still local → only the copilot page (id 2) is orphaned.
    await _prune_orphan_skills(fake, "TS", {"mcp:github"}, dry_run=False)
    assert deleted == ["2"]
    await fake.stop()


@pytest.mark.asyncio
async def test_prune_dry_run_deletes_nothing() -> None:
    from crewlet.confluence.import_cli import _prune_orphan_skills

    deleted: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET" and "search" in request.url.path:
            return httpx.Response(
                200, json={"results": [_skill_page("2", "skill:copilot_delegation")]}
            )
        if request.method == "DELETE":
            deleted.append(request.url.path)
            return httpx.Response(204)
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await _prune_orphan_skills(fake, "TS", set(), dry_run=True)
    assert deleted == []
    await fake.stop()


@pytest.mark.asyncio
async def test_prune_leaves_marker_pages_without_key_label() -> None:
    """A marker-labelled page with no recognizable per-key label is
    ambiguous and must not be deleted."""
    from crewlet.agent.skills.labels import SKILL_MARKER_LABEL
    from crewlet.confluence.import_cli import _prune_orphan_skills

    deleted: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET" and "search" in request.url.path:
            return httpx.Response(
                200,
                json={
                    "results": [
                        {
                            "id": "9",
                            "title": "no-key",
                            "metadata": {
                                "labels": {"results": [{"name": SKILL_MARKER_LABEL}]}
                            },
                        }
                    ]
                },
            )
        if request.method == "DELETE":
            deleted.append(request.url.path)
            return httpx.Response(204)
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))
    await _prune_orphan_skills(fake, "TS", set(), dry_run=False)
    assert deleted == []
    await fake.stop()


@pytest.mark.asyncio
async def test_run_import_prune_deletes_orphan(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    """End-to-end: run_import(prune=True) collects the local skill keys and
    deletes the import-managed skill page whose key is no longer present."""
    deleted: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET" and "search" in request.url.path:
            # prune's find_all_by_label → one orphan (copilot).
            return httpx.Response(
                200,
                json={"results": [_skill_page("42", "skill:copilot_delegation")]},
            )
        if request.method == "DELETE":
            deleted.append(request.url.path.rsplit("/", 1)[-1])
            return httpx.Response(204)
        return httpx.Response(405)

    fake = _FakeTransport(httpx.MockTransport(handler))

    async def _fake_build(config_path: Path) -> Any:
        return fake

    async def _space_ok(transport: Any, space_key: str) -> bool:
        return True

    async def _noop_import(*_a: Any, **_k: Any) -> None:
        return None

    monkeypatch.setattr(pages, "build_transport", _fake_build)
    monkeypatch.setattr(pages, "check_space_exists", _space_ok)
    monkeypatch.setattr(import_cli, "import_one", _noop_import)

    # github.md is the only local skill (key mcp:github), so the copilot
    # page is orphaned and pruned.
    await import_cli.run_import(
        config_path=tmp_path / "cfg.yaml",
        paths=[EXAMPLES_DIR / "github.md"],
        update=True,
        dry_run=False,
        prune=True,
        skill_space="TS",
    )
    assert deleted == ["42"]
    await fake.stop()
