"""Run the dashboard's JavaScript tests under ``node``.

The dashboard is a zero-build ES-module app served straight off disk, so
it has no package.json, no node_modules, and no JavaScript test runner —
and it had no tests at all, which is how a whole-page re-render on every
websocket envelope and a swallowed LLM failure both shipped.

The suites in ``js/`` are plain ES modules using a three-function
harness and a vendored DOM (``js/dom.mjs``), so running them needs
nothing but a ``node`` binary.  GitHub's Ubuntu runners ship one, so this
executes in CI without a setup step.

Which is the part that needs a guard.  Node is an *implicit* dependency
of the workflow — nothing declares it and nothing pins it — so the day a
runner image stops shipping one, every suite here turns into a skip and
the build stays green.  That is the failure mode CONTRIBUTING calls out
by name ("a skip is not a pass"), and it is worth more here than
anywhere else in the tree: this is the dashboard's *only* coverage.  Its
Python side has none, because there is barely any Python in it.

So the skip is local-only.  On a laptop without node the dashboard's
tests are somebody else's problem and the rest of the suite still runs;
under CI, a missing binary fails instead, and says what to install.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest
import yaml

JS_DIR = Path(__file__).parent / "js"
_CI_WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "ci.yml"
# The suites, discovered rather than listed, so a new ``*.test.mjs`` runs
# without anyone remembering to register it here.
SUITES = sorted(JS_DIR.glob("*.test.mjs"))

# Wall-clock cap per suite.  Each is a few hundred pure-function
# assertions and finishes in well under a second; this only exists so a
# hung node process fails the build instead of stalling it.
TIMEOUT_SECONDS = 60

node = shutil.which("node")

# Every CI provider worth the name sets this, GitHub Actions included.
IN_CI = os.environ.get("CI", "").lower() in {"1", "true", "yes"}

# The per-suite tests skip without node, in CI as well as locally — the
# assertion below is what makes CI red, and it says so once. Letting all
# two dozen fail instead buries the one legible message under two dozen
# copies of `assert None is not None`.
needs_node = pytest.mark.skipif(
    node is None,
    reason="node is not installed; the dashboard's JS suites need it",
)


def test_node_is_available_in_ci() -> None:
    """CI must actually run these, not report a skip as a pass.

    The pytest summary line prints one character per test, so a whole
    subsystem going quiet looks like ``ssssss`` in a wall of dots and
    reads as green. This is the assertion that makes it red instead.
    """
    if not IN_CI:
        pytest.skip("not running under CI")
    assert node is not None, (
        "node is not on PATH, so every dashboard suite would skip and the "
        "build would still pass. Add a step that installs it — the runner "
        "image used to ship one, and evidently no longer does."
    )


def test_ci_declares_the_node_dependency() -> None:
    """The workflow must ASK for node, not hope the image has one.

    The guard above turns a missing binary into a red build, which is the
    protection. This is the other half: a step that installs node when
    the image stops shipping it, so the red build is one somebody can
    fix by reading the log rather than by bisecting runner images.
    """
    workflow = yaml.safe_load(_CI_WORKFLOW.read_text())
    steps = workflow["jobs"]["test"]["steps"]
    runs = " ".join(str(step.get("run", "")) for step in steps)
    assert "node --version" in runs, (
        "the CI `test` job no longer ensures node is installed; without it "
        f"all {len(SUITES)} dashboard suites depend on an undeclared "
        "property of the runner image"
    )


def test_suites_are_discovered() -> None:
    """Guard the glob: an empty run must not look like a passing one."""
    assert SUITES, f"no *.test.mjs suites found in {JS_DIR}"


@needs_node
@pytest.mark.parametrize("suite", SUITES, ids=lambda p: p.stem)
def test_dashboard_js_suite(suite: Path) -> None:
    """Every assertion in one JavaScript suite passes."""
    assert node is not None
    result = subprocess.run(
        [node, str(suite)],
        capture_output=True,
        text=True,
        timeout=TIMEOUT_SECONDS,
        check=False,
    )
    output = result.stdout + result.stderr
    assert result.returncode == 0, f"{suite.name} failed:\n{output}"
    assert "passed" in output, f"{suite.name} produced no result line:\n{output}"
