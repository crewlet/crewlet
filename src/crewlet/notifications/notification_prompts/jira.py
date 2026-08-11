"""Jira notification prompt — tool-agnostic task descriptions."""

from __future__ import annotations

import json
from typing import TYPE_CHECKING, Any

from crewlet.notifications.notification_prompts.base import NotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification

# Fields whose ``fromString`` / ``toString`` are paragraphs of prose.
# Rendering them inline produces a wall of text on every edit; we
# replace the diff with a one-line "X was updated" summary instead.
# ``description`` change-events also tend to occur alongside comments
# (Jira often re-emits the full description on adjacent changes),
# so trimming this here is the dominant source of noise reduction.
_DIFF_HEAVY_FIELDS = {"description", "environment"}

# Fields whose ``from`` / ``to`` carry an Atlassian account ID rather
# than a free-form string.  When ``fromString`` / ``toString`` are
# absent (Atlassian routinely omits them on assignee transitions where
# one side is null), we resolve the raw account ID against the agent
# handle registry so the prompt shows ``agent-cto`` instead of
# ``712020:00000000-...``.
_ACCOUNT_ID_FIELDS = {"assignee", "reporter", "creator", "watchers"}


class JiraNotificationPrompt(NotificationPrompt):
    """Builds prompts for Jira-originated events.

    Instructions describe *intent* (e.g. "move the issue to In Progress")
    rather than specific tool names.
    """

    def build(
        self,
        notification: InboundNotification,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        meta = notification.metadata
        issue_key = self._issue_key_of(meta, notification.subject)

        # ── Event Summary ──
        description = (
            f"A Jira event has been routed to you.\n\n"
            f"**Title:** {notification.subject}\n"
        )

        # Event metadata (routing & event context) + a one-line lead
        # naming what kind of change happened, so the agent doesn't
        # have to infer it from the changelog shape.
        event_action = meta.get("event_type", "") or notification.source_event_type
        actor = meta.get("actor_name", "") or notification.sender
        # Forge bodies carry only ``{accountId: ...}`` for actors --
        # no displayName, no name -- so ``actor`` from the parser is
        # routinely empty.  Resolve the actor's account ID through
        # the registry so the prompt names a known teammate by handle
        # instead of leaving the lead-line author blank.
        actor_account_id = meta.get("actor_account_id", "")
        if not actor and actor_account_id and handle_registry is not None:
            resolved = self._resolve_account_id(handle_registry, actor_account_id)
            if resolved:
                actor = resolved
        timestamp = meta.get("timestamp", "")

        meta_lines = []
        lead = self._event_lead(event_action, actor)
        if lead:
            meta_lines.append(f"- {lead}")
        if event_action:
            meta_lines.append(f"- **Event:** {event_action}")
        if actor:
            meta_lines.append(f"- **Actor:** {actor}")
        if timestamp:
            meta_lines.append(f"- **Timestamp:** {timestamp}")

        routed_via = meta.get("routed_via", "")
        if routed_via:
            meta_lines.append(f"- **Routed via:** {routed_via}")

        if meta_lines:
            description += "\n## Event Metadata\n" + "\n".join(meta_lines) + "\n"

        # Render changelog so the agent knows exactly what changed.
        changelog_section = self._format_changelog(
            meta.get("changelog", ""), handle_registry=handle_registry
        )
        if changelog_section:
            description += changelog_section

        # Body handling.  For ``comment_created`` the body is the
        # actual comment text — keep it.  For ``issue_updated`` it is
        # the issue's full description, which the changelog summary
        # already covered as "description was updated"; including the
        # full body again would be a duplicated wall of text.
        if notification.body and self._should_include_body(event_action):
            description += f"\n**Details:**\n{notification.body}\n"

        # ── Get Full Context ──
        if issue_key:
            description += (
                f"\n## Get Full Context"
                f"\nUse your Jira tools to fetch the full details "
                f"for **{issue_key}** (type, status, priority, "
                f"reporter, description, acceptance criteria, "
                f"linked issues) before deciding on next steps. "
                f"Do not act on partial information.\n"
            )

        # ── Lead-fallback framing ──
        # When the only reason this lead received the event is that
        # the issue had no specific recipient (no assignee, no other
        # watcher, no @mention beyond the creator), the lead is the
        # default owner -- but only by fallback, not by personal
        # involvement.  Surface that explicitly so the lead delegates
        # rather than silently absorbing the work.
        description += self._why_routed_block(meta)

        # ── Execution Guidance ──
        # Platform-level Jira mechanics. Role-specific behavioral
        # guidelines (ownership, tone, quality bar) belong in the
        # role's ``behavioral_guidelines``, not here.
        description += self._handling_guidance(meta)

        return description

    @staticmethod
    def _handling_guidance(meta: dict[str, Any]) -> str:
        """The static "How to Handle This" block.

        The verbose-decline rule (step 5) is rendered only for
        routings that represent a direct ask of the recipient
        (``assignee``, ``mention``, or an empty reason).  Watcher
        routings are not direct asks -- watchers receive events
        because they once interacted, not because someone is
        asking them now -- so the rule is omitted to avoid
        triggering running-commentary noise.  Project-lead-fallback
        routings are also excluded here because the dedicated
        ``Why You Received This`` block already lays out the
        delegate / take-it / escalate decision.
        """
        routed_via = meta.get("routed_via", "")
        direct_ask = routed_via in ("", "assignee", "mention")

        parts = [
            "\n## How to Handle This"
            "\n1. **Are you the right owner?** If the issue has an"
            " assignee that isn't you and isn't on your team, observe"
            " and skip — don't comment unless you hold decision-"
            "blocking information the assignee cannot see."
            "\n2. **Has this already been addressed?** Read the existing"
            " comments before commenting. If your prior comment (or a"
            " teammate's) already covers the question, don't restate"
            " it."
            "\n3. **If acting:** set the issue to in-progress, ensure"
            " the assignee field is set (use `lookup_colleague` to resolve"
            " a teammate's Jira account ID if needed), and post ONE"
            " substantive completion summary when done."
            ' Avoid running commentary ("starting work", "still'
            ' working") — those are noise.'
            "\n4. **If unclear:** comment mentioning the reporter for"
            " clarification — don't guess."
        ]
        if direct_ask:
            parts.append(
                "\n5. **If you were directly assigned or @mentioned but"
                " have decided not to act** (out of scope, wrong owner,"
                " already handled): do NOT skip silently. Post a brief"
                " comment naming the right owner (and reassign if you"
                " can), or pointing at where the work is being tracked."
                " If you can't identify the right owner, @mention your"
                " own manager (named in your identity prompt) so they"
                " can route it. Leaving a direct assignment / mention"
                " unanswered looks like the message was lost."
            )
            last_num = 6
        else:
            last_num = 5
        parts.append(
            f"\n{last_num}. **Never** post internal thinking, status"
            ' acknowledgements, or "I agree with X" as comments.'
            " Substance only. Stay focused on this specific issue.\n"
        )
        return "".join(parts)

    @staticmethod
    def _why_routed_block(meta: dict[str, Any]) -> str:
        """Return a "Why You Received This" section when the lead got
        this event by routing fallback rather than personal involvement.

        Returned empty for ``watcher`` / ``assignee`` / ``mention``
        routings -- those carry their own signal of why the recipient
        is involved, so the general guidance is enough.  The block is
        only added for ``project_lead_fallback`` (see
        ``JiraTransport.handle_webhook``), where the only reason the
        lead received the event is that no specific recipient applied.
        """
        if meta.get("routed_via", "") != "project_lead_fallback":
            return ""
        project = meta.get("project", "")
        project_phrase = (
            f"the **{project}** Jira project" if project else "this Jira project"
        )
        return (
            "\n## Why You Received This"
            f"\nThis event was routed to you because you are the lead of"
            f" the team that owns {project_phrase}, and the issue has no"
            " specific recipient — there is no assignee, no other"
            " watcher, and no @mention beyond the creator. You are the"
            " default owner only by fallback, not because the work is"
            " yours. Since lead-fallback fires only when nobody else is"
            " involved, no one else is watching this issue — if you"
            " walk away silently, it goes nowhere."
            "\n\nDecide one of:"
            "\n- **Delegate** — assign the issue to the right teammate."
            " Use `lookup_colleague` to find them on your team and resolve"
            " their Jira account ID, then set the assignee. Future"
            " updates on this issue will route to them, not back to"
            " you."
            "\n- **Take it yourself** — only if the work clearly falls"
            " on you. Assign the issue to yourself so the routing"
            " reflects reality from now on."
            "\n- **Escalate** — if the issue is out of scope or you"
            " can't identify the right owner, hand it off to your own"
            " manager (named in your identity prompt): comment on the"
            " issue @mentioning them, or reassign the issue to them."
            " Either keeps the trail on the ticket so the work isn't"
            " lost.\n"
        )

    @staticmethod
    def _event_lead(event_action: str, actor: str) -> str:
        """One-line summary naming the event kind, e.g. "A comment was
        added by <actor>".

        Branching on ``event_action`` so the agent doesn't have to
        infer "what just happened" from the changelog shape — Jira's
        ``avi:jira:updated:issue`` covers comment-adds, description
        edits, status transitions, and assignee changes alike.
        """
        action = (event_action or "").lower()
        actor_text = f" by {actor}" if actor else ""
        # Forge maps the comment-add event to ``comment_created``
        # internally (the wire event is ``avi:jira:commented:issue``);
        # the comment-deleted case is the only other comment event we
        # ever see -- Forge does not emit comment edits.
        if "comment" in action and ("created" in action or "added" in action):
            return f"A comment was added{actor_text}."
        if "comment" in action and "deleted" in action:
            return f"A comment was deleted{actor_text}."
        if "issue" in action and "created" in action:
            return f"The issue was created{actor_text}."
        if "issue" in action and "updated" in action:
            return f"The issue was updated{actor_text}."
        if "issue" in action and "deleted" in action:
            return f"The issue was deleted{actor_text}."
        if "worklog" in action:
            return f"A worklog entry was logged{actor_text}."
        return ""

    @staticmethod
    def _should_include_body(event_action: str) -> bool:
        """Whether to render ``notification.body`` as ``**Details:**``.

        For ``comment_created`` the body is the comment text and is
        the actual signal.  For ``issue_updated`` it's the full
        description, which the changelog summary already covers --
        duplicating it would be a wall of text on every edit.
        """
        action = (event_action or "").lower()
        return "comment" in action

    @classmethod
    def _format_changelog(
        cls,
        changelog_raw: str,
        *,
        handle_registry: HandleRegistry | None = None,
    ) -> str:
        """Format serialized changelog items into a readable prompt section.

        Each item carries ``field`` plus ``from`` / ``fromString`` and
        ``to`` / ``toString``.  Jira normally populates the
        ``*String`` variants with display names; webhook bridges
        sometimes send them as ``null`` and only carry the raw IDs.
        Falling through to ``from`` / ``to`` keeps the line useful
        (an account ID is more informative than ``(none) → (none)``)
        and we drop a line entirely when *both* sides are empty after
        the fallback (the change carried no signal at all).

        For ``assignee`` / ``reporter`` / ``creator`` fields the
        ``from`` / ``to`` IDs are Atlassian account IDs.  When a
        ``handle_registry`` is supplied we resolve those to friendly
        handles in the rendered line, saving a ``lookup_colleague``
        round-trip from the agent for every event.
        """
        if not changelog_raw:
            return ""
        try:
            items: list[dict[str, Any]] = json.loads(changelog_raw)
        except (json.JSONDecodeError, TypeError):
            return ""
        if not items:
            return ""

        lines: list[str] = []
        for item in items:
            field = item.get("field", "unknown")

            if field in _DIFF_HEAVY_FIELDS:
                lines.append(f"- **{field}**: was updated")
                continue

            from_val = item.get("fromString") or item.get("from") or ""
            to_val = item.get("toString") or item.get("to") or ""
            if not from_val and not to_val:
                # Both sides empty after the fallback -- the changelog
                # entry carries no signal; skip rather than emit
                # ``(none) → (none)``.
                continue
            # Account-id fields: when we only have the raw ID (no
            # ``*String``) try the registry for a friendlier handle.
            if field in _ACCOUNT_ID_FIELDS and handle_registry is not None:
                if from_val and not item.get("fromString"):
                    from_val = (
                        cls._resolve_account_id(handle_registry, from_val) or from_val
                    )
                if to_val and not item.get("toString"):
                    to_val = cls._resolve_account_id(handle_registry, to_val) or to_val
            from_disp = from_val or "(none)"
            to_disp = to_val or "(none)"
            lines.append(f"- **{field}**: {from_disp} → {to_disp}")

        if not lines:
            return ""
        return "\n## What Changed\n" + "\n".join(lines) + "\n"

    @classmethod
    def _resolve_account_id(cls, registry: HandleRegistry, account_id: str) -> str:
        """Resolve a Jira account ID to a colleague label.

        Agents render as ``role (handle)``; human seats render as
        ``Name (handle, human colleague)`` — see
        :meth:`NotificationPrompt._party_label`.  Returns an empty
        string when the registry doesn't know the ID -- the caller
        falls back to the raw account ID, which is still strictly
        more informative than ``(none)``.
        """
        return cls._party_label(registry, "jira", account_id)

    @staticmethod
    def _extract_issue_key(title: str) -> str:
        """Extract a Jira issue key (e.g. 'POC-123') from the title."""
        for part in title.split():
            if "-" in part and part.split("-")[-1].isdigit():
                return part
        return ""

    def requires_recon(self, notification: InboundNotification) -> bool:
        """A Jira webhook is always a pointer: it names the issue that
        changed, and ``build()`` emits a ``## Get Full Context`` block
        telling the agent to fetch the full issue before acting.  Same
        condition ``build()`` gates that block on -- an identifiable
        issue key."""
        return bool(self._issue_key_of(notification.metadata, notification.subject))

    def conversation_key(self, metadata: dict[str, str], *, subject: str = "") -> str:
        """The issue is the conversation."""
        return self._issue_key_of(metadata, subject)

    @classmethod
    def _issue_key_of(cls, metadata: dict[str, str], subject: str) -> str:
        """The ONE place that maps a Jira event to its issue.

        Explicit ``issue_key`` metadata, else the key embedded in the
        subject line.  ``build``'s Get-Full-Context block,
        ``requires_recon``'s pointer decision, and inbox coalescing's
        ``conversation_key`` all resolve through here — if the webhook
        parser ever changes how the issue is carried, one fix covers
        recon, prompts, and coalescing together."""
        return metadata.get("issue_key", "") or cls._extract_issue_key(subject)

    def digest_entry_body(self, event_type: str, body: str) -> str:
        """Comments keep their body; field-update bodies are superseded.

        Mirrors ``_should_include_body``: on ``issue_updated`` events
        the body is the issue's full (now stale) description -- in a
        coalesced digest only the latest state matters, and it is
        rendered in full below the digest, so intermediate copies
        collapse to their event lead."""
        return body if self._should_include_body(event_type) else ""
