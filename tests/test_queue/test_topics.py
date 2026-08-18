"""The queue subject grammar.

One definition, because a producer and a consumer that disagree about a
topic name do not raise — the publish succeeds into a topic nobody
reads.  This test is the guard: it fails the build if a second
``crewlet.agent.…inbox`` f-string appears in the source tree.
"""

from __future__ import annotations

import pathlib
import re

from crewlet.queue.topics import (
    AGENT_INBOX_PREFIX,
    AGENT_INBOX_SUFFIX,
    agent_inbox_topic,
)


def test_the_subject_is_prefix_handle_suffix():
    assert agent_inbox_topic("alice") == "crewlet.agent.alice.inbox"
    assert agent_inbox_topic("alice").startswith(AGENT_INBOX_PREFIX)
    assert agent_inbox_topic("alice").endswith(AGENT_INBOX_SUFFIX)


def test_hyphenated_handles_pass_through_unchanged():
    # Handles are ``[a-z0-9][a-z0-9-]*`` — no escaping, no case folding.
    assert agent_inbox_topic("qa-lead") == "crewlet.agent.qa-lead.inbox"


def test_an_empty_handle_yields_no_subject():
    """Not ``crewlet.agent..inbox``.

    That is a real, publishable topic with no subscriber, so a caller
    that lost the handle would silently drop the event instead of
    failing.  Returning ``""`` makes the caller's mistake visible.
    """
    assert agent_inbox_topic("") == ""


def test_no_other_module_builds_the_subject_by_hand():
    """The reason this module exists.

    Nine call sites formatted this f-string before it was unified; each
    one was a chance for a routing change to reach some producers and
    not others.  Docstrings are allowed to name the shape.
    """
    src = pathlib.Path(__file__).resolve().parents[2] / "src" / "crewlet"
    pattern = re.compile(r'f"crewlet\.agent\.\{')
    offenders = [
        str(path.relative_to(src))
        for path in src.rglob("*.py")
        if path.name != "topics.py" and pattern.search(path.read_text())
    ]
    assert offenders == []
