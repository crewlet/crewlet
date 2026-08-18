"""Run the dashboard's JavaScript tests under ``node``.

The dashboard is a zero-build ES-module app served straight off disk, so
it has no package.json, no node_modules, and no JavaScript test runner —
and it had no tests at all, which is how a whole-page re-render on every
websocket envelope and a swallowed LLM failure both shipped.

The suites in ``js/`` are plain ES modules using a three-function
harness and a vendored DOM (``js/dom.mjs``), so running them needs
nothing but a ``node`` binary.  GitHub's Ubuntu runners ship one, so this
executes in CI without a setup step; a machine without node skips
instead of failing, since the dashboard's Python side is unaffected by
its absence.
"""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

JS_DIR = Path(__file__).parent / "js"
# The suites, discovered rather than listed, so a new ``*.test.mjs`` runs
# without anyone remembering to register it here.
SUITES = sorted(JS_DIR.glob("*.test.mjs"))

# Wall-clock cap per suite.  Each is a few hundred pure-function
# assertions and finishes in well under a second; this only exists so a
# hung node process fails the build instead of stalling it.
TIMEOUT_SECONDS = 60

node = shutil.which("node")

pytestmark = pytest.mark.skipif(
    node is None,
    reason="node is not installed; the dashboard's JS suites need it",
)


def test_suites_are_discovered() -> None:
    """Guard the glob: an empty run must not look like a passing one."""
    assert SUITES, f"no *.test.mjs suites found in {JS_DIR}"


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
