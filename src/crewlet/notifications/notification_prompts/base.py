"""NotificationPrompt base class and generic fallback prompt.

Each source-specific prompt extends ``NotificationPrompt`` and implements
``build()`` to produce a tool-agnostic task description from an
``InboundNotification``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification


class NotificationPrompt:
    """Base class for source-specific notification prompts.

    Subclasses override ``build()`` to produce a clear, actionable
    description that tells the agent *what to do* without referring
    to specific tool names.
    """

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        """Build a prompt for the given notification.

        ``handle_registry`` is optional; when supplied, source-specific
        builders can use it to rewrite raw external IDs (Slack user
        IDs, Jira account IDs) into friendly handles in the rendered
        prompt -- saves a ``lookup_colleague`` round-trip on every event.

        Returns a markdown-formatted description of the notification.
        """
        return self._generic_description(notification)

    def requires_recon(self, notification: InboundNotification) -> bool:
        """True when the notification is a *pointer*, not the context.

        A webhook that says "POC-518 got a comment" or "review PR #42"
        only tells the agent *where* to look -- the agent must fetch
        the issue / read the thread / pull the diff before it has the
        real context.  When this is ``True`` the Plan-phase
        relevance-filter prefetches (personal memory, relevant
        knowledge) skip their aux-LLM call: filtering against a bare
        pointer is near-guaranteed low-value, and the planner is
        already told to re-query after recon.  See
        ``docs/concepts/agent-learning.md`` (Thin-trigger gate).

        The base prompt returns ``False`` -- the generic builder dumps
        the full ``notification.body`` into the prompt, so the body
        *is* the context.  Source-specific builders override when
        their webhooks are pointers.
        """
        return False

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """Source-local identity of the conversation this event belongs to.

        Inbox batching partitions an agent's pending events by
        conversation (see :mod:`crewlet.notifications.coalesce`): a Jira
        issue, a Slack thread, a Confluence page, a GitHub PR.  Events
        sharing a key are merged into one digest trigger; events whose
        key is empty are never coalesced.

        The returned key is source-*local* ("POC-7", "C42:1718.003") --
        the caller namespaces it with the notification source, so two
        sources can never collide.  The base prompt returns ``""``
        (unknown sources have no derivable conversation identity);
        source-specific builders override using the same metadata keys
        their ``build()`` already reads.
        """
        return ""

    def digest_entry_body(self, event_type: str, body: str) -> str:
        """Body to render for one constituent line of a coalesced digest.

        Per-source supersede hook: when several same-conversation events
        merge into one digest, sources whose intermediate bodies carry
        no signal of their own (e.g. Jira ``issue_updated`` re-emitting
        the full stale description on every edit) return ``""`` so the
        digest line collapses to the event lead ("the issue was
        updated") -- only the latest state, rendered in full below the
        digest, matters.  Messages and comments always keep their body.

        The base prompt keeps every body -- for Slack-like sources each
        message is the signal.
        """
        return body

    @staticmethod
    def _party_label(
        registry: HandleRegistry | None,
        transport: str,
        external_id: str,
    ) -> str:
        """Render a colleague label for an external sender/actor ID.

        Agents render as ``Role (handle)`` — the role name is what
        the LLM recognises from prior conversation, the handle is the
        canonical identifier for ``lookup_colleague`` / ``a2a_ask``
        follow-ups.  Human seats render as
        ``Name (handle, human colleague)`` so the agent immediately
        knows the counterparty is a person: replies come on the
        external surface, asynchronously, and ``a2a_ask`` won't reach
        them.

        Returns ``""`` when the ID is unknown — the caller falls back
        to the raw external ID.
        """
        if registry is None or not external_id:
            return ""
        try:
            party = registry.resolve_party_external(transport, external_id)
        except Exception:
            return ""
        if party is None:
            return ""
        if party.is_human:
            if party.name and party.handle:
                return f"{party.name} ({party.handle}, human colleague)"
            return f"{party.name or party.handle} (human colleague)"
        if party.name and party.handle:
            return f"{party.name} ({party.handle})"
        return party.handle or party.name

    @staticmethod
    def _generic_description(notification: InboundNotification) -> str:
        """Fallback description for any source."""
        source = notification.source
        subject = notification.subject or f"{source} notification"

        meta_lines = ""
        if notification.metadata:
            meta_lines = "\n".join(
                f"  {k}: {v}" for k, v in notification.metadata.items() if v
            )

        return (
            f"You received a notification from {source}: "
            f"{subject}\n\n"
            f"**Message:**\n{notification.body}\n\n"
            f"**Notification metadata:**\n{meta_lines}\n\n"
            f"## Evaluate Before Acting\n"
            f"1. **Were you actually asked to do something?** Read the"
            f" verb, not just whether your name appears. Being mentioned"
            f' in passing (e.g. "check with @you on this") or named as'
            f" the subject of a message addressed to someone else is"
            f" NOT a request directed at you.\n"
            f"2. **If you were not asked** (informational, FYI,"
            f" addressee is someone else, passing reference) → stay"
            f" silent. Silence beats noise.\n"
            f"3. **If you were asked but have decided not to act**"
            f" (out of scope, already handled, wrong person, declining)"
            f" → do NOT skip silently. Post a brief reply on the"
            f" originating channel explaining why — leaving a direct"
            f" ask unanswered looks like the message was lost.\n"
        )
