"""Guards on the distribution metadata Crewlet publishes to PyPI.

None of this is exercised by importing the package, and most of it only fails
at ``twine upload`` time — on the tag that publishes, after which a bad release
can be yanked but never replaced.  These tests move those failures onto the
pull request that causes them.

The release mechanics themselves live in ``RELEASING.md``.
"""

from __future__ import annotations

import re
import tomllib
from pathlib import Path
from typing import Any

import pytest
import yaml

import crewlet

_REPO_ROOT = Path(__file__).resolve().parents[2]
_PYPROJECT = _REPO_ROOT / "pyproject.toml"
_README = _REPO_ROOT / "README.md"
_CHANGELOG = _REPO_ROOT / "CHANGELOG.md"
_CI_WORKFLOW = _REPO_ROOT / ".github" / "workflows" / "ci.yml"
_RELEASE_WORKFLOW = _REPO_ROOT / ".github" / "workflows" / "release.yml"

# PEP 440, restricted to the release forms this project actually cuts.
_VERSION_RE = re.compile(r"^\d+\.\d+\.\d+((a|b|rc)\d+|\.post\d+|\.dev\d+)?$")


@pytest.fixture(scope="module")
def pyproject() -> dict[str, Any]:
    return tomllib.loads(_PYPROJECT.read_text())


class TestVersion:
    def test_version_is_pep440(self) -> None:
        assert _VERSION_RE.match(crewlet.__version__), (
            f"{crewlet.__version__!r} is not a version PyPI will accept"
        )

    def test_version_is_single_sourced_from_the_package(
        self, pyproject: dict[str, Any]
    ) -> None:
        """The build backend must read the version out of ``__init__.py``.

        A literal ``version = "..."`` in pyproject.toml would be a second place
        to bump, and the two silently drift: ``crewlet --version`` would report
        one number while the wheel on PyPI carries another.
        """
        assert "version" in pyproject["project"]["dynamic"]
        assert "version" not in pyproject["project"]
        assert (
            pyproject["tool"]["hatch"]["version"]["path"] == "src/crewlet/__init__.py"
        )

    def test_changelog_documents_the_current_version(self) -> None:
        """The GitHub Release body is cut from this section, so it must exist."""
        changelog = _CHANGELOG.read_text()
        assert f"## [{crewlet.__version__}]" in changelog, (
            f"CHANGELOG.md has no section for {crewlet.__version__}"
        )


class TestLicense:
    def test_no_license_classifier_alongside_the_spdx_expression(
        self, pyproject: dict[str, Any]
    ) -> None:
        """PEP 639: a ``License ::`` classifier plus ``License-Expression`` is
        a combination PyPI is specified to reject.  ``license = "MIT"`` is an
        SPDX expression, so the classifier must not come back.
        """
        project = pyproject["project"]
        assert isinstance(project["license"], str), (
            "license must stay an SPDX expression string, not a table"
        )
        offenders = [c for c in project["classifiers"] if c.startswith("License ::")]
        assert not offenders, (
            f"remove the deprecated license classifier(s) {offenders}; "
            f"the license is already declared as {project['license']!r}"
        )

    def test_declared_license_files_exist(self, pyproject: dict[str, Any]) -> None:
        """PyPI rejects a distribution whose ``License-File`` is not in it."""
        for name in pyproject["project"]["license-files"]:
            assert (_REPO_ROOT / name).is_file(), f"missing license file: {name}"


class TestExtras:
    def test_all_extra_is_the_union_of_the_runtime_extras(
        self, pyproject: dict[str, Any]
    ) -> None:
        """``crewlet[all]`` is documented as the kitchen sink.  A new extra that
        is not folded in here makes that documentation quietly wrong.
        """
        extras = pyproject["project"]["optional-dependencies"]
        runtime = {
            requirement
            for name, requirements in extras.items()
            if name not in {"all", "dev"}
            for requirement in requirements
        }
        assert set(extras["all"]) == runtime

    def test_no_empty_extras(self, pyproject: dict[str, Any]) -> None:
        """An extra with no requirements installs nothing but advertises a
        capability toggle that does not exist.
        """
        empty = [
            name
            for name, requirements in pyproject["project"][
                "optional-dependencies"
            ].items()
            if not requirements
        ]
        assert not empty, f"extras with no dependencies: {empty}"


# The jobs that must run on every supported interpreter, not merely the ones
# that happen to today: `test` runs the suite from the source tree, and
# `package-install` installs the built wheel the way PyPI serves it.  Naming
# them makes deleting a fan-out a deliberate edit to this list rather than a
# silent narrowing of what CI covers.
_FAN_OUT_JOBS = {"test", "package-install"}


def _ci_python_matrices() -> dict[str, list[str]]:
    """Every CI job that fans out over interpreters, by job name."""
    workflow = yaml.safe_load(_CI_WORKFLOW.read_text())
    return {
        name: job["strategy"]["matrix"]["python-version"]
        for name, job in workflow["jobs"].items()
        if "python-version" in job.get("strategy", {}).get("matrix", {})
    }


class TestPythonSupport:
    def test_the_jobs_that_must_fan_out_still_do(self) -> None:
        """Guards the shape, not just the contents.  Without this, deleting a
        job's matrix would leave the agreement check below trivially satisfied
        by whatever jobs remain.
        """
        missing = sorted(_FAN_OUT_JOBS - set(_ci_python_matrices()))
        assert not missing, (
            f"CI jobs {missing} no longer fan out over python-version; "
            "restore the matrix, or update _FAN_OUT_JOBS if the job was renamed"
        )

    def test_every_ci_matrix_covers_the_same_interpreters(self) -> None:
        """A version added to one fan-out job and forgotten in the other is a
        support claim only half of CI actually backs.
        """
        matrices = _ci_python_matrices()
        distinct = {tuple(versions) for versions in matrices.values()}
        assert len(distinct) == 1, f"CI matrices disagree: {matrices}"

    def test_classifiers_match_the_tested_interpreters(
        self, pyproject: dict[str, Any]
    ) -> None:
        """A ``Programming Language :: Python :: X.Y`` classifier is a support
        claim.  It may only name interpreters the CI matrices actually run.
        """
        tested = set(next(iter(_ci_python_matrices().values())))
        claimed = {
            c.rsplit(" ", 1)[1]
            for c in pyproject["project"]["classifiers"]
            if re.fullmatch(r"Programming Language :: Python :: \d+\.\d+", c)
        }
        assert claimed == tested, (
            f"classifiers claim {sorted(claimed)} but CI tests {sorted(tested)}"
        )

    def test_requires_python_floor_matches_the_oldest_tested(
        self, pyproject: dict[str, Any]
    ) -> None:
        oldest = min(
            next(iter(_ci_python_matrices().values())),
            key=lambda v: tuple(int(p) for p in v.split(".")),
        )
        assert pyproject["project"]["requires-python"] == f">={oldest}"


def _apply_readme_substitutions(text: str, pyproject: dict[str, Any]) -> str:
    """Run the configured hatch-fancy-pypi-readme rewrites over ``text``."""
    hook = pyproject["tool"]["hatch"]["metadata"]["hooks"]["fancy-pypi-readme"]
    for substitution in hook["substitutions"]:
        text = re.sub(substitution["pattern"], substitution["replacement"], text)
    return text


def _relative_references(text: str) -> set[str]:
    """Every link or image target in ``text`` that is not absolute."""
    found = re.findall(r'(?:href|src)="([^"]+)"', text) + re.findall(
        r"\]\(([^)]+)\)", text
    )
    return {
        ref
        for ref in found
        if not ref.startswith(("http://", "https://", "#", "mailto:", "data:"))
    }


class TestReadme:
    def test_readme_is_built_by_the_substitution_hook(
        self, pyproject: dict[str, Any]
    ) -> None:
        hook = pyproject["tool"]["hatch"]["metadata"]["hooks"]["fancy-pypi-readme"]
        assert "readme" in pyproject["project"]["dynamic"]
        assert "readme" not in pyproject["project"]
        assert hook["content-type"] == "text/markdown"
        assert [f["path"] for f in hook["fragments"]] == ["README.md"]

    def test_every_relative_reference_is_absolutised_for_pypi(
        self, pyproject: dict[str, Any]
    ) -> None:
        """PyPI has no repository to resolve relative links against, so any that
        survive the rewrite render as dead links and broken images on the
        project page.  A new link style the patterns do not match fails here.
        """
        rewritten = _apply_readme_substitutions(_README.read_text(), pyproject)
        assert not _relative_references(rewritten)

    def test_relative_references_point_at_files_that_exist(self) -> None:
        """The rewrite turns these into github.com URLs verbatim — a path that
        is wrong in the repo becomes a 404 on PyPI too.
        """
        broken = sorted(
            ref
            for ref in _relative_references(_README.read_text())
            if not (_REPO_ROOT / ref.split("#", 1)[0]).exists()
        )
        assert not broken, f"README links to missing paths: {broken}"


class TestReleaseWorkflow:
    def test_publishes_with_trusted_publishing_and_no_token(self) -> None:
        """The PyPI trusted publisher is bound to this workflow's *filename*,
        and the OIDC token it mints needs ``id-token: write``.  Renaming the
        file or dropping the permission breaks publishing at upload time.
        """
        workflow = yaml.safe_load(_RELEASE_WORKFLOW.read_text())
        publish = workflow["jobs"]["publish-pypi"]
        assert publish["permissions"]["id-token"] == "write"
        assert publish["environment"]["name"] == "pypi"
        uses = [step.get("uses", "") for step in publish["steps"]]
        assert any(u.startswith("pypa/gh-action-pypi-publish@") for u in uses)
        assert "password" not in _RELEASE_WORKFLOW.read_text(), (
            "trusted publishing takes no API token"
        )

    def test_tag_trigger_matches_the_documented_release_command(self) -> None:
        # `on` is parsed as the YAML boolean True unless quoted in the file.
        workflow = yaml.safe_load(_RELEASE_WORKFLOW.read_text())
        triggers = workflow[True] if True in workflow else workflow["on"]
        assert triggers["push"]["tags"] == ["v*"]
