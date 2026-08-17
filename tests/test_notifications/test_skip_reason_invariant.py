"""The chat transports report why an event was skipped through a shared
mutable attribute (``transport.last_skip_reason``) that the notification
service reads immediately after awaiting ``handle_event``.

That is only safe because of an invariant the code never states: **no
``await`` may occur between assigning the reason and returning.**  One
transport instance serves every agent, so if a parse suspended after
setting the reason, another agent's parse could overwrite it before the
first caller read it — and the ``NotificationSkipped`` event would name
the wrong agent's reason.

Nothing enforces that today, and a single innocuous ``await`` added
before a ``return`` would break it silently: the attribution is only
wrong under concurrency, so it would never show up in a single-agent
test.  This module is that enforcement.

The alternative — returning the reason instead of stashing it — is the
structurally safer design, but it churns the transports' public parse
signature and every caller for a defect that does not currently exist.
Guarding the invariant is the proportionate fix; if a transport ever
genuinely needs to await after deciding to skip, the right move is to
change the signature, not to relax this test.
"""

from __future__ import annotations

import ast
import pathlib

import pytest

_TRANSPORTS = [
    ("slack", "src/crewlet/notifications/transports/slack.py"),
    ("mattermost", "src/crewlet/notifications/transports/mattermost.py"),
]

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]


def _timeline(func: ast.AsyncFunctionDef) -> list[tuple[str, int]]:
    """Ordered (kind, lineno) events for awaits, skip-reason sets, returns."""
    events: list[tuple[str, int]] = []

    class Scan(ast.NodeVisitor):
        def visit_Await(self, node: ast.Await) -> None:
            events.append(("await", node.lineno))
            self.generic_visit(node)

        def visit_Assign(self, node: ast.Assign) -> None:
            for target in node.targets:
                if (
                    isinstance(target, ast.Attribute)
                    and target.attr == "last_skip_reason"
                ):
                    events.append(("set", node.lineno))
            self.generic_visit(node)

        def visit_Return(self, node: ast.Return) -> None:
            events.append(("return", node.lineno))
            self.generic_visit(node)

    Scan().visit(func)
    return sorted(events, key=lambda event: event[1])


@pytest.mark.parametrize("name,path", _TRANSPORTS)
def test_no_await_between_setting_a_skip_reason_and_returning(name, path):
    source = (_REPO_ROOT / path).read_text()
    tree = ast.parse(source)
    handlers = [
        node
        for node in ast.walk(tree)
        if isinstance(node, ast.AsyncFunctionDef) and node.name == "handle_event"
    ]
    assert handlers, f"{name}: no async handle_event found in {path}"

    for handler in handlers:
        events = _timeline(handler)
        assert any(kind == "set" for kind, _ in events), (
            f"{name}: handle_event never records a skip reason — if the "
            "reporting mechanism changed, update or delete this guard"
        )
        for index, (kind, line) in enumerate(events):
            if kind != "set":
                continue
            for next_kind, next_line in events[index + 1 :]:
                if next_kind == "return":
                    break
                if next_kind == "await":
                    pytest.fail(
                        f"{name}: last_skip_reason is set at line {line} but "
                        f"an await follows at line {next_line} before the "
                        "function returns. One transport instance serves "
                        "every agent, so suspending here lets a concurrent "
                        "parse overwrite the reason and mis-attribute the "
                        "NotificationSkipped event. Either move the "
                        "assignment after the await, or return the reason "
                        "instead of stashing it on the instance."
                    )
