"""Queue subject names — the one place a Crewlet topic string is built.

An agent's inbox subject (``crewlet.agent.{handle}.inbox``) is the
engine's routing primitive: the notification service publishes to it,
the scheduler fires into it, an A2A ask wakes a colleague through it,
the sandbox coordinator resumes a suspended turn on it, and the engine
subscribes one consumer per seat to it.  Nine call sites formatted that
f-string by hand, which is nine chances for a producer and a consumer
to disagree about a name that has to match exactly — a mismatch is not
an error anywhere, it is a message published to a topic nobody reads.

The grammar lives here so there is exactly one definition, and here
rather than beside the parties because it is a *queue* fact: the
handle is the routing key, not the identity.  This module imports
nothing from ``crewlet`` so any layer can use it.
"""

from __future__ import annotations

#: Subject prefix for per-agent inboxes.
AGENT_INBOX_PREFIX = "crewlet.agent."
#: Subject suffix for per-agent inboxes.
AGENT_INBOX_SUFFIX = ".inbox"
#: Subject suffix for per-agent sandbox control.
AGENT_CONTROL_SUFFIX = ".control"


def agent_inbox_topic(handle: str) -> str:
    """Return the inbox subject for the seat with ``handle``.

    ``agent_inbox_topic("alice")`` → ``"crewlet.agent.alice.inbox"``.

    The handle is the *seat's* handle, which every process derives from
    the org — never a process-local instance id.  An empty handle
    returns an empty subject rather than a topic named after nothing:
    callers must treat that as "not routable" instead of publishing to
    ``crewlet.agent..inbox``, a real topic that no consumer subscribes
    to and that would swallow the event silently.
    """
    if not handle:
        return ""
    return f"{AGENT_INBOX_PREFIX}{handle}{AGENT_INBOX_SUFFIX}"


def agent_inbox_group(handle: str) -> str:
    """Return the durable consumer group for the seat's inbox.

    One group per seat, so membership *is* ownership: the node that
    attaches a consumer to ``agent-{handle}`` is the node that receives
    that seat's work.  Nothing computes "which node" — routing falls out
    of who subscribed.

    Empty for an empty handle, for the same reason
    :func:`agent_inbox_topic` is.
    """
    if not handle:
        return ""
    return f"agent-{handle}"


def agent_control_topic(handle: str) -> str:
    """Return the seat's sandbox-control subject.

    ``agent_control_topic("alice")`` → ``"crewlet.agent.alice.control"``.

    Separate from the inbox because a detached sandbox run PAUSES the
    inbox: a completion riding the inbox would queue behind the very
    pause it exists to lift.  Attached and detached alongside the inbox,
    so a completion reaches the seat's owner and only its owner.
    """
    if not handle:
        return ""
    return f"{AGENT_INBOX_PREFIX}{handle}{AGENT_CONTROL_SUFFIX}"


def agent_control_group(handle: str) -> str:
    """Return the durable consumer group for the seat's control topic."""
    if not handle:
        return ""
    return f"agent-{handle}-control"
