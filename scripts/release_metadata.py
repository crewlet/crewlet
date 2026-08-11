#!/usr/bin/env python3
"""Release metadata for the tag that publishes crewlet to PyPI.

``.github/workflows/release.yml`` shells out to this for the two things it has
to know about a release before it builds anything: what version the package
reports, and whether that version agrees with the tag being pushed. The GitHub
Release body is not here — GitHub generates it from the merged pull requests,
shaped by ``.github/release.yml``.

It lives in ``scripts/`` rather than as heredocs inside the workflow so that
``tests/test_packaging`` can exercise it on every pull request. Everything
here otherwise runs for the first time on the tag that publishes, and by then
a bad release can be yanked but never replaced.

Deliberately standard-library only, and importable without crewlet installed:
the workflow runs it on a bare checkout, before any dependency is installed.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parent.parent
_VERSION_SOURCE = "src/crewlet/__init__.py"

# PEP 440, restricted to the release forms this project actually cuts.
VERSION_RE = re.compile(r"^\d+\.\d+\.\d+((a|b|rc)\d+|\.post\d+|\.dev\d+)?$")

# The suffixes that mean "do not put users on this by default". ``.post1`` is
# deliberately absent: a post-release re-publishes packaging metadata for a
# release that already happened, so it *is* the current one.
_PRERELEASE_RE = re.compile(r"((a|b|rc)\d+|\.dev\d+)$")


def read_version(root: Path = _REPO_ROOT) -> str:
    """The version the package reports — the single source of truth."""
    source = (root / _VERSION_SOURCE).read_text()
    match = re.search(r'^__version__ = "([^"]+)"$', source, re.MULTILINE)
    if match is None:
        raise SystemExit(f"No __version__ found in {_VERSION_SOURCE}")
    return match.group(1)


def is_prerelease(version: str) -> bool:
    """Whether GitHub must flag this release rather than call it "Latest".

    ``pip`` already refuses to resolve a pre-release without ``--pre``, so an
    unflagged ``v0.2.0rc1`` would leave the Releases page — and the
    ``/releases/latest`` redirect every "get the current version" script
    follows — advertising a release candidate that PyPI would not serve.
    """
    return _PRERELEASE_RE.search(version) is not None


def release_problems(version: str, tag: str) -> list[str]:
    """Every reason this release must not be built, in report order."""
    problems: list[str] = []

    if not VERSION_RE.match(version):
        problems.append(
            f"__version__ {version!r} is not a version PyPI will accept. "
            "Use a PEP 440 release, pre-release, post-release or dev version."
        )

    # A tag run must name the version the code actually reports, or the
    # artifact and the git history disagree forever.
    if tag and tag.lstrip("v") != version:
        problems.append(
            f"tag {tag!r} does not match __version__ {version!r}. "
            f"Bump {_VERSION_SOURCE}, or delete and re-push the tag."
        )

    return problems


def _cmd_version(args: argparse.Namespace) -> int:
    """Emit the workflow outputs the later jobs read."""
    version = read_version(args.root)
    prerelease = is_prerelease(version)
    print(f"version={version}")
    print(f"prerelease={'true' if prerelease else 'false'}")
    # stdout is redirected into $GITHUB_OUTPUT, so the human-readable line goes
    # to the log instead.
    print(
        f"Package version: {version} (prerelease: {prerelease})",
        file=sys.stderr,
    )
    return 0


def _cmd_verify(args: argparse.Namespace) -> int:
    version = read_version(args.root)
    problems = release_problems(version, args.tag)
    if problems:
        formatted = "\n".join(f"  - {problem}" for problem in problems)
        print(f"Release checks failed:\n{formatted}", file=sys.stderr)
        return 1
    print(f"Version {version} is consistent with the tag.")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--root",
        type=Path,
        default=_REPO_ROOT,
        help="repository root to read (default: this script's repository)",
    )
    commands = parser.add_subparsers(dest="command", required=True)

    version = commands.add_parser(
        "version", help="print version= and prerelease= as GITHUB_OUTPUT lines"
    )
    version.set_defaults(func=_cmd_version)

    verify = commands.add_parser(
        "verify", help="check the package version against the pushed tag"
    )
    verify.add_argument(
        "--tag", default="", help="the pushed tag, empty for a non-tag run"
    )
    verify.set_defaults(func=_cmd_verify)

    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
