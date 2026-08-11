"""Backend-neutral markdown-document helpers shared by both publishers.

The Confluence and Plane import CLIs (and the page codecs behind them)
share one authoring model: local ``.md`` files with optional YAML
frontmatter, routed by frontmatter ``trigger`` (Tool Skill vs knowledge
doc), titled by the first ATX ``# H1`` (or a frontmatter ``title``
override).  This module owns everything about that model that is
backend-free:

- :func:`split_optional_frontmatter` / :func:`extract_title` /
  :func:`parse_doc_file` -- the tolerant knowledge-doc parse
  (frontmatter optional, H1 stripped from the published body);
- :func:`classify` -- the skill-vs-doc routing rule;
- :func:`collect_md_files` -- recursive ``.md`` collection;
- :func:`render_markdown` -- the single markdown -> XHTML-serialised
  HTML conversion both wire formats use (Confluence storage format and
  Plane ``description_html`` both accept it);
- :func:`html_to_text` -- the deliberately lossy HTML -> plain-text
  flatten the decode paths use (skill prose is read by an LLM, not a
  human);
- :func:`dump_frontmatter_yaml` -- the operator-friendly YAML dump
  (multi-line strings as literal ``|`` blocks) both page encoders embed.

Nothing here imports Confluence or Plane code; the backend-specific
wire formats live in :mod:`crewlet.agent.skills.confluence_codec` /
:mod:`crewlet.confluence.knowledge` and :mod:`crewlet.plane.plane_codec`.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any, Literal

import yaml
from bs4 import BeautifulSoup, Tag
from markdown import markdown as _md_to_html

_FRONTMATTER_RE = re.compile(
    r"\A---\s*\n(.*?\n)---\s*\n?(.*)\Z",
    re.DOTALL,
)
"""Matches a leading ``---\\n … \\n---`` YAML frontmatter block.

Same shape the skill parser uses, but consumed by
:func:`split_optional_frontmatter`, which tolerates a *missing* block
(returning empty frontmatter) — unlike the skill parser, which mandates
one.
"""

_H1_RE = re.compile(r"\A\s*#[ \t]+(?P<title>[^\n]+?)[ \t]*(?:\n|\Z)")
"""Matches the first leading ATX ``# `` heading line of a body.

Anchored at the start (``\\A``) so it only ever strips the *leading* H1
— a ``# `` further down the body is real content and is left untouched.
``[^\\n]+?`` captures the heading text up to (but not including) the
newline, with trailing spaces/tabs trimmed.
"""


class DocParseError(ValueError):
    """Raised when a knowledge-doc file can't be parsed or has no title."""


def split_optional_frontmatter(text: str) -> tuple[dict[str, Any], str]:
    """Split a file into ``(frontmatter, body)``, tolerating no frontmatter.

    If ``text`` opens with a ``---\\n … \\n---`` block, parse the YAML
    mapping and return ``(mapping, remaining_body)``.  Otherwise return
    ``({}, text)`` unchanged — a knowledge doc with no frontmatter is the
    common case under the directory convention.

    A *present* block whose YAML is malformed (or isn't a mapping) raises
    :class:`DocParseError`; an absent block never does.
    """
    match = _FRONTMATTER_RE.match(text)
    if not match:
        return {}, text
    raw_yaml = match.group(1)
    body = match.group(2)
    try:
        frontmatter = yaml.safe_load(raw_yaml)
    except yaml.YAMLError as exc:
        raise DocParseError(f"invalid YAML in frontmatter: {exc}") from exc
    if frontmatter is None:
        frontmatter = {}
    if not isinstance(frontmatter, dict):
        raise DocParseError("frontmatter must be a YAML mapping")
    return frontmatter, body


def extract_title(frontmatter: dict[str, Any], body: str) -> tuple[str, str]:
    """Resolve ``(title, body_without_leading_h1)`` for a knowledge doc.

    The title is ``frontmatter["title"]`` when present and non-empty,
    otherwise the text of the body's first leading ``# `` ATX heading.
    The leading ``# `` heading line is **always** stripped from the
    returned body when one is present, so the published page doesn't
    repeat the title the knowledge base shows separately — this holds
    even when a frontmatter ``title`` overrides the H1 text.

    Raises :class:`DocParseError` when no title can be determined (no
    frontmatter ``title`` and no leading H1).
    """
    fm_title = str(frontmatter.get("title", "")).strip()

    # ``_H1_RE`` is anchored at the start and tolerates leading
    # whitespace/newlines, so a file that opens with blank lines still
    # has its leading H1 recognised; matching is confined to the first
    # heading so a ``# `` mid-body stays as real content.
    stripped_body = body
    h1_title = ""
    h1_match = _H1_RE.match(body)
    if h1_match:
        h1_title = h1_match.group("title").strip()
        stripped_body = body[h1_match.end() :].lstrip("\n")

    title = fm_title or h1_title
    if not title:
        raise DocParseError(
            "knowledge doc has no title: add a leading '# Heading' or a "
            "frontmatter 'title:'"
        )
    return title, stripped_body


def parse_doc_file(text: str) -> tuple[dict[str, Any], str, str]:
    """Parse a knowledge-doc file into ``(frontmatter, title, body)``.

    Composes :func:`split_optional_frontmatter` and :func:`extract_title`:
    optional frontmatter is split off, the title is taken from the
    frontmatter override or the leading H1, and that H1 is stripped from
    the returned body.

    The target *container* (Confluence space / Plane project) is **not**
    read here — it comes from the source file's parent directory
    (resolved by the import CLIs).  Raises :class:`DocParseError` on
    malformed frontmatter or a missing title.
    """
    frontmatter, body = split_optional_frontmatter(text)
    title, doc_body = extract_title(frontmatter, body)
    return frontmatter, title, doc_body


def classify(frontmatter: dict[str, Any]) -> Literal["skill", "doc"]:
    """Route a parsed frontmatter to ``"skill"`` / ``"doc"``.

    - ``trigger`` present → ``"skill"`` (Tool Skill; its directory is
      ignored, it lands in the Tool Skills container).
    - otherwise → ``"doc"`` (knowledge doc; its container comes from the
      file's parent directory and its title from the first ``# H1`` or a
      frontmatter ``title`` override).

    A doc candidate may still be skipped downstream if it has no
    determinable title — that check lives in :func:`parse_doc_file`, not
    here.
    """
    if "trigger" in frontmatter:
        return "skill"
    return "doc"


def collect_md_files(paths: list[Path]) -> list[Path]:
    """Collect every ``*.md`` file under ``paths`` (recursive, deduped).

    A path may be a single ``.md`` file or a directory (walked
    recursively).  Returns a stable, sorted, duplicate-free list.
    """
    found: set[Path] = set()
    for path in paths:
        if path.is_file() and path.suffix == ".md":
            found.add(path.resolve())
        elif path.is_dir():
            for md in path.rglob("*.md"):
                found.add(md.resolve())
    return sorted(found)


def render_markdown(body_markdown: str) -> str:
    """Render markdown to XHTML-serialised HTML.

    The single rendering call every page encoder shares — the
    Confluence skill codec (:func:`~crewlet.agent.skills.confluence_codec.encode_page`)
    and knowledge-doc encoder
    (:func:`~crewlet.confluence.knowledge.encode_doc`), and their Plane
    analogs in :mod:`crewlet.plane.plane_codec`.  XHTML output serves
    both wire formats: Confluence's storage format *requires* it, and
    Plane's ``description_html`` sanitizer accepts it unchanged.
    Keeping it in one place means every published page uses identical
    markdown → HTML conversion.
    """
    return _md_to_html(
        body_markdown,
        extensions=["fenced_code", "tables"],
        output_format="xhtml",
    )


def html_to_text(html: str) -> str:
    """Convert an HTML page body to plain text.

    Best-effort: lists become newline-separated bullets with ``- ``
    prefix, headings render as bare text, ``<p>`` becomes a blank-line
    separator.  Unknown / backend-specific elements (Confluence
    ``ac:*`` macros, Plane editor wrappers) are stripped to their text
    content.  Good enough for LLM prompts; if operators want exact
    markdown fidelity they should keep the source files in version
    control and re-import on changes.
    """
    soup = BeautifulSoup(html, "html.parser")
    return _walk(soup).strip()


def _walk(node: Any) -> str:
    if isinstance(node, str):
        return node
    if not isinstance(node, Tag):
        # NavigableString or similar — render via str().
        return str(node)
    name = node.name or ""
    children = "".join(_walk(c) for c in node.children)
    if name in {"p", "div"}:
        return f"{children.strip()}\n\n"
    if name.startswith("h") and name[1:].isdigit():
        return f"{children.strip()}\n\n"
    if name in {"li"}:
        return f"- {children.strip()}\n"
    if name in {"ul", "ol"}:
        return f"{children}\n"
    if name in {"br"}:
        return "\n"
    if name in {"strong", "b", "em", "i", "code", "a"}:
        return children
    if name in {"pre"}:
        return f"{children}\n\n"
    # Backend-specific wrappers: render their text content only.
    return children


class _FrontmatterDumper(yaml.SafeDumper):
    """SafeDumper that emits multi-line strings as literal ``|`` blocks.

    PyYAML's default str representer falls back to single-quoted style
    with awkward line-folding when a scalar contains ``\\n``, producing
    output like::

        summary: 'If a stakeholder told you a standing team rule

          (not a one-off preference), share it via Slack.

          '

    Operators look at this in the page editor and rightly find it
    ugly.  Switching multi-line scalars to literal block style matches
    the way the source ``examples/tool-skills/*.md`` files are
    authored::

        summary: |-
          If a stakeholder told you a standing team rule (not a
          one-off preference), share it via Slack.
    """


def _represent_multiline_strings(dumper: yaml.SafeDumper, data: str) -> Any:
    if "\n" in data:
        return dumper.represent_scalar("tag:yaml.org,2002:str", data, style="|")
    return dumper.represent_scalar("tag:yaml.org,2002:str", data)


_FrontmatterDumper.add_representer(str, _represent_multiline_strings)


def dump_frontmatter_yaml(frontmatter: dict[str, Any]) -> str:
    """Dump a frontmatter mapping to operator-friendly YAML.

    Key order is preserved (``sort_keys=False``) so the page's YAML box
    reads like the authored source file, unicode passes through
    unescaped, and multi-line strings render as literal ``|`` blocks
    (see :class:`_FrontmatterDumper`).  Trailing whitespace is stripped
    so encoders control their own block delimiters.
    """
    return yaml.dump(
        frontmatter,
        Dumper=_FrontmatterDumper,
        sort_keys=False,
        default_flow_style=False,
        allow_unicode=True,
    ).rstrip()


__all__ = [
    "DocParseError",
    "classify",
    "collect_md_files",
    "dump_frontmatter_yaml",
    "extract_title",
    "html_to_text",
    "parse_doc_file",
    "render_markdown",
    "split_optional_frontmatter",
]
