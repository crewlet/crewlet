"""Built-in notification sources — webhook receiver and polling."""

from __future__ import annotations

import asyncio
import contextlib
import json
import re
from collections.abc import Callable, Coroutine
from typing import Any

from crewlet._logging import get_logger
from crewlet.notifications.protocol import InboundNotification

# Callback type for WebhookSource/PollingSource.  The EventQueue is
# the preferred integration path for new code.
NotificationCallback = Callable[[InboundNotification], Coroutine[Any, Any, None]]

logger = get_logger("notifications.sources")

# Type for webhook parsers
WebhookParser = Callable[
    [str, dict[str, Any], dict[str, str]],
    Coroutine[Any, Any, InboundNotification | None],
]


# ADF block-level node types that should end with a newline so the
# flattened text preserves paragraph / list / heading separation.
_ADF_BLOCK_TYPES = frozenset(
    {
        "paragraph",
        "heading",
        "blockquote",
        "bulletList",
        "orderedList",
        "listItem",
        "codeBlock",
        "rule",
        "panel",
        "doc",
    }
)


def _flatten_adf(node: Any) -> str:
    """Flatten an Atlassian Document Format (ADF) node to plain text.

    Jira Cloud sends ``comment.body`` (and ``fields.description``) as
    an ADF tree -- ``{type, content: [...], text, attrs}`` -- not as a
    plain string or wiki markup, so the raw value cannot be assigned
    to :attr:`InboundNotification.body` (typed ``str``).  Walk the
    tree concatenating text leaves, ending block-level nodes with a
    newline so paragraphs and list items don't run together, and
    rendering mentions / emoji / hardBreaks in a way that keeps the
    intent readable for the agent.

    Plain strings pass through unchanged so callers can hand any
    body shape (string, ADF dict, ``None``) to this helper without a
    type check at the call site.
    """
    if node is None:
        return ""
    if isinstance(node, str):
        return node
    if not isinstance(node, dict):
        return str(node)

    node_type = node.get("type", "")

    if node_type == "text":
        return node.get("text", "") or ""
    if node_type == "mention":
        attrs = node.get("attrs", {}) or {}
        text = attrs.get("text", "") or attrs.get("displayName", "")
        if text:
            return text if text.startswith("@") else f"@{text}"
        account_id = attrs.get("id", "") or ""
        return f"[~accountid:{account_id}]" if account_id else ""
    if node_type == "emoji":
        attrs = node.get("attrs", {}) or {}
        return attrs.get("shortName", "") or attrs.get("text", "") or ""
    if node_type == "hardBreak":
        return "\n"

    content = node.get("content")
    if not isinstance(content, list):
        return ""
    inner = "".join(_flatten_adf(child) for child in content)
    if node_type in _ADF_BLOCK_TYPES:
        return inner + "\n"
    return inner


def _coerce_text(value: Any) -> str:
    """Coerce a Jira body value (string OR ADF dict) to plain text.

    Jira Cloud increasingly returns ADF for any rich-text field;
    older Server / DC instances and some webhook bridges still send
    wiki-markup strings.  Normalise here so downstream consumers
    don't branch on the type.
    """
    if isinstance(value, dict):
        return _flatten_adf(value).strip()
    return value if isinstance(value, str) else ""


def extract_adf_mention_ids(value: Any) -> list[str]:
    """Walk an ADF tree and collect every ``mention`` node's account ID.

    Jira Cloud emits ``avi:jira:commented:issue`` webhooks with the
    comment body as ADF, and any ``@<user>`` references appear as
    structured ``{"type": "mention", "attrs": {"id": "..."}}`` nodes.
    Routing has to honour those mentions because Jira's watcher list
    does NOT auto-include mentioned users -- without this lookup
    a non-watcher who got @'d would never receive the notification.

    Returns the list of account IDs in document order with duplicates
    preserved (the caller dedupes against its own delivery set).
    Strings / non-dict roots / empty trees return ``[]`` safely.
    """
    ids: list[str] = []

    def _walk(node: Any) -> None:
        if not isinstance(node, dict):
            return
        if node.get("type") == "mention":
            attrs = node.get("attrs") or {}
            account_id = attrs.get("id") or ""
            if account_id:
                ids.append(account_id)
        content = node.get("content")
        if isinstance(content, list):
            for child in content:
                _walk(child)

    _walk(value)
    return ids


class WebhookSource:
    """Notification source that dispatches webhook payloads to agents.

    Does not run its own HTTP server — call ``handle_webhook()`` from
    your API endpoint. Parsers transform raw webhook payloads into
    ``InboundNotification`` objects::

        source = WebhookSource()
        source.register_parser("jira", parse_jira_webhook)
        source.register_parser("github", parse_github_webhook)
    """

    name: str = "webhook"
    _MAX_PENDING: int = 1000

    def __init__(self) -> None:
        self._callback: NotificationCallback | None = None
        self._parsers: dict[str, WebhookParser] = {}
        self._pending: list[InboundNotification] = []
        self._stopped = False

    def register_parser(self, source_name: str, parser: WebhookParser) -> None:
        """Register a parser for a webhook source.

        The parser converts raw request body/headers into an
        ``InboundNotification``.
        """
        self._parsers[source_name] = parser

    async def start(self, callback: NotificationCallback) -> None:
        self._callback = callback
        self._stopped = False
        # Process any notifications that arrived before start
        for notification in self._pending:
            await callback(notification)
        self._pending.clear()
        logger.info(
            "webhook_source_ready",
            parsers=", ".join(self._parsers.keys()) or "none",
        )

    async def stop(self) -> None:
        self._callback = None
        self._stopped = True
        self._pending.clear()
        logger.info("webhook_source_stopped")

    async def handle_webhook(
        self,
        source_name: str,
        body: dict[str, Any],
        headers: dict[str, str] | None = None,
    ) -> bool:
        """Process an incoming webhook.

        Called by the API layer (or directly in tests) when a
        webhook arrives. Returns True if successfully processed.
        """
        parser = self._parsers.get(source_name)
        if parser is None:
            logger.warning(
                "webhook_parser_not_registered",
                source_name=source_name,
            )
            return False

        try:
            notification = await parser(source_name, body, headers or {})
        except Exception as exc:
            logger.error(
                "webhook_parse_failed",
                source_name=source_name,
                error=str(exc),
            )
            return False

        if notification is None:
            return False

        if self._callback is not None:
            try:
                await self._callback(notification)
            except Exception as exc:
                logger.error(
                    "webhook_callback_error",
                    source_name=source_name,
                    error=str(exc),
                )
                return False
        elif self._stopped:
            logger.warning("webhook_dropped_after_stop")
        else:
            if len(self._pending) >= self._MAX_PENDING:
                logger.warning(
                    "webhook_pending_queue_full",
                    max_pending=self._MAX_PENDING,
                )
                self._pending.pop(0)
            self._pending.append(notification)
        return True


# --- Built-in webhook parsers ---


async def parse_jira_webhook(
    source_name: str,
    body: dict[str, Any],
    headers: dict[str, str],
) -> InboundNotification | None:
    """Parse a Jira webhook payload into a notification.

    Handles common Jira events: issue_created, issue_assigned,
    issue_updated, comment_created.
    """
    event_type = body.get("webhookEvent", "")
    issue = body.get("issue", {})
    fields = issue.get("fields", {})
    changelog_items = body.get("changelog", {}).get("items", [])

    # Determine recipient email and account ID
    # Cloud uses accountId; Data Center uses name (username)
    assignee = fields.get("assignee", {}) or {}
    recipient_email = assignee.get("emailAddress", "")
    assignee_account_id = assignee.get("accountId") or assignee.get("name", "")

    comment = body.get("comment", {})

    sender_data = body.get("user", {}) or {}
    # Jira comment_created webhooks have no top-level "user" — fall back
    # to comment.author so the notification includes the actual actor.
    if not sender_data and comment:
        sender_data = comment.get("author", {}) or {}
    sender = sender_data.get("displayName", sender_data.get("name", ""))

    subject = fields.get("summary", "")
    issue_key = issue.get("key", "")

    body_text = ""
    # Comment lifecycle events surface the comment text itself.  For
    # ``comment_deleted`` the body is what was deleted, which is still
    # the relevant signal for the agent.  Forge does not emit a
    # comment-edit event, so there is no ``comment_updated`` case.
    # Jira Cloud sends ``comment.body`` and ``fields.description`` as
    # ADF (Atlassian Document Format) JSON rather than plain strings,
    # so coerce through ``_coerce_text`` rather than assigning the raw
    # dict to ``InboundNotification.body`` (which is typed ``str``).
    _comment_events = {"comment_created", "comment_deleted"}
    if event_type in _comment_events and comment:
        body_text = _coerce_text(comment.get("body", ""))
    elif fields.get("description"):
        body_text = _coerce_text(fields["description"])

    # Serialize changelog items for downstream prompt building.
    # Each item has: field, fieldtype, from, fromString, to, toString.
    changelog_json = json.dumps(changelog_items) if changelog_items else ""

    logger.debug(
        "jira_webhook_parsed",
        event_type=event_type,
        issue_key=issue_key,
        assignee_name=assignee.get("displayName", ""),
        recipient_email=recipient_email,
        assignee_account_id=assignee_account_id,
        sender=sender,
        changelog_fields=[item.get("field", "") for item in changelog_items],
    )

    # Forge bodies inject ``user.accountId`` from ``atlassianId``;
    # surface it explicitly so downstream renderers can resolve the
    # actor through the agent handle registry when ``displayName`` is
    # empty (which it always is for Forge -- Forge never includes
    # display names on user references).
    actor_account_id = sender_data.get("accountId", "")

    metadata: dict[str, str] = {
        "issue_key": issue_key,
        "event_type": event_type,
        "project": fields.get("project", {}).get("key", ""),
        "assignee_account_id": assignee_account_id,
        "actor_name": sender,
        "actor_account_id": actor_account_id,
        "timestamp": str(body.get("timestamp", "")),
    }
    if changelog_json:
        metadata["changelog"] = changelog_json

    return InboundNotification(
        source="jira",
        source_event_type=event_type,
        recipient_email=recipient_email,
        sender=sender,
        subject=f"[{issue_key}] {subject}" if issue_key else subject,
        body=body_text,
        metadata=metadata,
    )


async def parse_github_webhook(
    source_name: str,
    body: dict[str, Any],
    headers: dict[str, str],
) -> InboundNotification | None:
    """Parse a GitHub webhook payload into a notification.

    Handles: pull_request (all actions — review_requested and assigned
    extract the direct target; other actions like opened, synchronize,
    ready_for_review, closed fall back to PR assignees/reviewers),
    issues (assigned), issue_comment.
    """
    event_type = headers.get("x-github-event", "")
    action = body.get("action", "")
    full_event = f"{event_type}.{action}" if action else event_type

    recipient_email = ""
    sender = ""
    subject = ""
    body_text = ""
    metadata: dict[str, str] = {"event_type": full_event}

    sender_data = body.get("sender", {})
    sender = sender_data.get("login", "")

    if event_type == "pull_request":
        pr = body.get("pull_request", {})
        subject = pr.get("title", "")
        body_text = pr.get("body", "") or ""
        metadata["pr_number"] = str(pr.get("number", ""))
        metadata["repo"] = body.get("repository", {}).get("full_name", "")

        if action == "review_requested":
            reviewer = body.get("requested_reviewer", {})
            recipient_email = reviewer.get("email", "")
            if reviewer.get("login"):
                metadata["github_login"] = reviewer["login"].lower()
        elif action == "assigned":
            assignee = body.get("assignee", {})
            recipient_email = assignee.get("email", "")
            if assignee.get("login"):
                metadata["github_login"] = assignee["login"].lower()

        # Fallback: for other actions (opened, synchronize, closed,
        # ready_for_review, etc.) try the first PR assignee, then
        # the first requested reviewer.
        if not recipient_email and "github_login" not in metadata:
            for source_list in (
                pr.get("assignees") or [],
                pr.get("requested_reviewers") or [],
            ):
                for user in source_list:
                    if not user:
                        continue
                    if user.get("email"):
                        recipient_email = user["email"]
                    if user.get("login"):
                        metadata["github_login"] = user["login"].lower()
                    if recipient_email or "github_login" in metadata:
                        break
                if recipient_email or "github_login" in metadata:
                    break

    elif event_type == "issues":
        issue = body.get("issue", {})
        subject = issue.get("title", "")
        body_text = issue.get("body", "") or ""
        metadata["issue_number"] = str(issue.get("number", ""))
        metadata["repo"] = body.get("repository", {}).get("full_name", "")

        if action == "assigned":
            assignee = body.get("assignee", {})
            recipient_email = assignee.get("email", "")

    elif event_type == "issue_comment":
        issue = body.get("issue", {})
        comment_data = body.get("comment", {})
        subject = issue.get("title", "")
        body_text = comment_data.get("body", "") or ""
        metadata["issue_number"] = str(issue.get("number", ""))
        metadata["repo"] = body.get("repository", {}).get("full_name", "")
        # Notify the assignee about the comment
        assignee = issue.get("assignee", {}) or {}
        recipient_email = assignee.get("email", "")

    return InboundNotification(
        source="github",
        source_event_type=full_event,
        recipient_email=recipient_email,
        sender=sender,
        subject=subject,
        body=body_text,
        metadata=metadata,
    )


_GITLAB_MENTION_RE = re.compile(
    r"(?<![\w/])@([a-zA-Z0-9](?:[a-zA-Z0-9._-]*[a-zA-Z0-9])?)"
)


def extract_gitlab_mentions(text: str) -> list[str]:
    """Collect ``@username`` tokens from a GitLab note/description body.

    GitLab sends comment/description bodies as raw markdown with **no**
    parsed-mention array (unlike Jira's ADF ``mention`` nodes), so the
    router recovers mentions by scanning the text.  Returns lowercased
    usernames in document order with duplicates removed; the caller
    intersects them against the registered GitLab usernames so arbitrary
    ``@`` text (email-style ``a@b``, ``@here``) never fans out to a real
    agent.  Leading ``@`` preceded by a word char or ``/`` is skipped so
    ``foo@bar`` / path fragments don't match.
    """
    seen: dict[str, None] = {}
    for match in _GITLAB_MENTION_RE.finditer(text or ""):
        seen.setdefault(match.group(1).lower(), None)
    return list(seen)


def _gitlab_usernames(users: Any) -> list[str]:
    """Lowercased ``username`` values from a GitLab users array."""
    out: list[str] = []
    if isinstance(users, list):
        for user in users:
            if isinstance(user, dict):
                name = user.get("username", "")
                if name:
                    out.append(name.lower())
    return out


def _gitlab_added(changes: dict[str, Any], key: str) -> list[str]:
    """Usernames newly present in ``changes[key].current`` vs ``previous``.

    GitLab's Issue / Merge Request ``update`` hook carries assignee and
    reviewer edits as ``changes.{assignees,reviewers} = {previous:[...],
    current:[...]}``.  A newly *added* assignee/reviewer is the routing
    target — someone removed from the list is not.
    """
    entry = changes.get(key)
    if not isinstance(entry, dict):
        return []
    previous = set(_gitlab_usernames(entry.get("previous")))
    return [u for u in _gitlab_usernames(entry.get("current")) if u not in previous]


#: Async ``(project_id, kind, iid) → lowercased usernames`` lookup for
#: participants-based routing; ``kind`` is ``"issue"`` or
#: ``"merge_request"``.  Built by
#: :func:`crewlet.gitlab.client.build_participants_fetcher`.
GitLabParticipantsFetcher = Callable[[int, str, int], Coroutine[Any, Any, list[str]]]


async def parse_gitlab_webhook(
    source_name: str,
    body: dict[str, Any],
    headers: dict[str, str],
    known_usernames: set[str] | None = None,
    participants_fetcher: GitLabParticipantsFetcher | None = None,
) -> list[InboundNotification]:
    """Parse a GitLab webhook payload into per-recipient notifications.

    Unlike the single-recipient GitHub parser this returns a **list**:
    one comment can @-mention several agents, and an update can add
    several assignees/reviewers.  Each notification targets exactly one
    GitLab username via ``metadata['gitlab_username']`` (resolved to an
    agent/human seat downstream); the ``event_type`` encodes *why* the
    agent is being pinged (``merge_request.review_requested``,
    ``issue.assigned``, ``note.mention`` …) so the prompt builder and
    inbox coalescer can act on the reason rather than re-deriving it.

    Routing mirrors GitLab's own notification semantics, in two layers:

    - **Directed events** target exactly the named party from the
      payload: assignee/reviewer additions (``changes`` diffs), explicit
      ``@mentions`` (note bodies AND issue/MR descriptions — new-mention
      diff on ``update``), and ``pipeline.failed`` (the actor owns the
      fix).
    - **Thread activity** — comments and state changes (close / reopen /
      merge / approvals) — fans out to the issue/MR **participants**
      (author + assignees + reviewers + commenters + previously-mentioned
      users), exactly the set GitLab itself would notify.  Participants
      are not in webhook payloads, so this layer needs
      ``participants_fetcher`` (one REST lookup per event, wired from
      ``integrations.gitlab.token``); without it — or when the lookup
      fails — routing degrades to the payload-derived assignees, so
      directed events are never at risk.

    Mentions stay explicitly extracted rather than inferred from
    participation, for two reasons: a mention is a *directed ask* and
    gets a tailored prompt (participation can't distinguish "this note
    pings you" from "you once commented"), and GitLab materialises new
    mentions into the participants list via a background job, so the
    lookup can race the webhook — text extraction can't.

    ``known_usernames`` (registered GitLab usernames, agents ∪ human
    seats) gates both mention and participant fan-out: only parties the
    engine can route to are targeted, so outsiders never produce
    undeliverable notifications.  Handled hooks: Issue, Merge Request,
    Note, Pipeline, Emoji.
    """
    object_kind = body.get("object_kind", "")
    attrs = body.get("object_attributes", {}) or {}
    actor = (body.get("user", {}) or {}).get("username", "")
    project = body.get("project", {}) or {}
    project_path = project.get("path_with_namespace", "")
    changes = body.get("changes", {}) or {}

    # (username, reason, suppress_self) targets in priority order; first
    # reason per username wins so a mentioned assignee pings once, as a
    # mention.  ``suppress_self`` drops a target that equals the actor —
    # right for interactive events (don't ping yourself about your own
    # comment / self-assignment) but NOT for ``pipeline.failed``, where
    # the actor IS the owner who needs to know the build broke.
    targets: list[tuple[str, str, bool]] = []

    def add(usernames: list[str], reason: str, *, suppress_self: bool = True) -> None:
        for username in usernames:
            targets.append((username, reason, suppress_self))

    def known(usernames: list[str]) -> list[str]:
        if known_usernames is None:
            return usernames
        return [u for u in usernames if u in known_usernames]

    def description_mentions(reason: str) -> None:
        """Directed pings from an issue/MR description.

        On ``open``/``reopen`` every description mention is a fresh ping;
        on ``update`` only mentions *added* by the edit count (GitLab's
        own semantics — re-saving a description doesn't re-notify).
        """
        if action == "update":
            entry = changes.get("description")
            if not isinstance(entry, dict):
                return
            previous = set(extract_gitlab_mentions(entry.get("previous") or ""))
            current = extract_gitlab_mentions(entry.get("current") or "")
            fresh = [u for u in current if u not in previous]
        else:
            fresh = extract_gitlab_mentions(attrs.get("description", "") or "")
        add(known(fresh), reason)

    subject = ""
    body_text = ""
    iid_key = ""
    iid_value = ""
    url = attrs.get("url", "")
    action = attrs.get("action", "")

    # Thread-activity fan-out request, set by the branches below when the
    # event is the kind GitLab notifies participants about.
    fan_kind = ""
    fan_iid = 0
    fan_reason = ""

    if object_kind == "issue":
        subject = attrs.get("title", "")
        body_text = attrs.get("description", "") or ""
        iid_key, iid_value = "issue_iid", str(attrs.get("iid", ""))
        if action == "update":
            add(_gitlab_added(changes, "assignees"), "issue.assigned")
            description_mentions("issue.mention")
        elif action in ("open", "reopen"):
            add(_gitlab_usernames(body.get("assignees")), "issue.assigned")
            description_mentions("issue.mention")
            # No participants fan-out on open: a fresh issue's
            # participants are the author (the actor) + assignees +
            # description mentions — all already targeted above.
        elif action == "close":
            # A human closing an issue an agent is working on must reach
            # the agent (stop working!).  Participants when available,
            # payload assignees as the degraded fallback.
            add(_gitlab_usernames(body.get("assignees")), "issue.close")
            fan_kind, fan_reason = "issue", "issue.close"
            fan_iid = int(attrs.get("iid") or 0)

    elif object_kind == "merge_request":
        subject = attrs.get("title", "")
        body_text = attrs.get("description", "") or ""
        iid_key, iid_value = "mr_iid", str(attrs.get("iid", ""))
        if action == "update":
            add(_gitlab_added(changes, "reviewers"), "merge_request.review_requested")
            add(_gitlab_added(changes, "assignees"), "merge_request.assigned")
            description_mentions("merge_request.mention")
        elif action in (
            "approval",
            "approved",
            "unapproval",
            "unapproved",
            "merge",
            "close",
        ):
            # State changes concern the whole thread — the author agent
            # reports back to its requester, the reviewer's follow-up
            # closes out.  Participants when available; payload assignees
            # as the degraded fallback (in the agent model the agent that
            # opened the MR assigns itself, so assignees reliably reaches
            # the owner — author username is not in the MR hook payload,
            # only author_id).
            add(_gitlab_usernames(body.get("assignees")), f"merge_request.{action}")
            fan_kind, fan_reason = "merge_request", f"merge_request.{action}"
            fan_iid = int(attrs.get("iid") or 0)
        elif action in ("open", "reopen"):
            add(
                _gitlab_usernames(body.get("reviewers")),
                "merge_request.review_requested",
            )
            add(_gitlab_usernames(body.get("assignees")), "merge_request.assigned")
            description_mentions("merge_request.mention")
            if action == "reopen":
                # Reopen wakes the thread, not just the named parties.
                fan_kind, fan_reason = "merge_request", "merge_request.reopen"
                fan_iid = int(attrs.get("iid") or 0)
        # Unknown/auto-merge actions → no notification.

    elif object_kind == "note":
        body_text = attrs.get("note", "") or ""
        noteable_type = (attrs.get("noteable_type", "") or "").lower()
        mr = body.get("merge_request", {}) or {}
        issue = body.get("issue", {}) or {}
        if noteable_type == "mergerequest" and mr:
            subject = mr.get("title", "")
            iid_key, iid_value = "mr_iid", str(mr.get("iid", ""))
            noteable_assignees = _gitlab_usernames(mr.get("assignees"))
            fan_kind, fan_iid = "merge_request", int(mr.get("iid") or 0)
        elif noteable_type == "issue" and issue:
            subject = issue.get("title", "")
            iid_key, iid_value = "issue_iid", str(issue.get("iid", ""))
            noteable_assignees = _gitlab_usernames(issue.get("assignees"))
            fan_kind, fan_iid = "issue", int(issue.get("iid") or 0)
        else:
            noteable_assignees = []
        add(known(extract_gitlab_mentions(body_text)), "note.mention")
        add([u for u in noteable_assignees if u != actor], "note.comment")
        fan_reason = "note.comment"

    elif object_kind == "pipeline":
        status = attrs.get("status", "")
        if status != "failed":
            return []
        mr = body.get("merge_request", {}) or {}
        subject = f"Pipeline {status}"
        if mr:
            iid_key, iid_value = "mr_iid", str(mr.get("iid", ""))
        # The agent whose push/action triggered the pipeline owns the fix.
        add([actor.lower()] if actor else [], "pipeline.failed", suppress_self=False)

    elif object_kind == "emoji":
        # Low-value; only routable when the awardable author username is
        # present.  Award events carry the awarder as ``user`` and the
        # awardable under its own key without a username, so there is no
        # reliable target — parse but do not fan out.
        return []
    else:
        return []

    # Thread-activity layer: everyone participating in the issue/MR hears
    # its comments and state changes — GitLab's own notification reach.
    # Purely additive over the directed targets above (the dedupe below
    # keeps the first, higher-signal reason per user), and best-effort: a
    # failed lookup degrades to the payload-derived routing.
    if participants_fetcher is not None and fan_kind and fan_iid and fan_reason:
        project_id = int(project.get("id") or body.get("project_id") or 0)
        if project_id:
            try:
                participants = await participants_fetcher(project_id, fan_kind, fan_iid)
            except Exception as exc:
                logger.warning(
                    "gitlab_participants_fetch_failed",
                    project_id=project_id,
                    kind=fan_kind,
                    iid=fan_iid,
                    error=str(exc),
                )
                participants = []
            add(known(participants), fan_reason)

    # Dedupe by username (first reason wins) and drop self-notifications
    # for interactive events.
    actor_lower = actor.lower()
    notifications: list[InboundNotification] = []
    emitted: set[str] = set()
    for username, reason, suppress_self in targets:
        if not username or username in emitted:
            continue
        if suppress_self and username == actor_lower:
            continue
        emitted.add(username)
        metadata: dict[str, str] = {
            "event_type": reason,
            "gitlab_username": username,
            "project": project_path,
            "actor_name": actor,
        }
        if iid_value:
            metadata[iid_key] = iid_value
        if url:
            metadata["url"] = url
        notifications.append(
            InboundNotification(
                source="gitlab",
                source_event_type=reason,
                recipient_email="",
                sender=actor,
                subject=subject,
                body=body_text,
                metadata=metadata,
            )
        )
    return notifications


class PollingSource:
    """Notification source that polls for new notifications.

    Periodically calls a user-provided async function to check
    for new notifications. Useful for RSS feeds or APIs that do not
    support webhooks.

    Example::

        async def check_feed() -> list[InboundNotification]:
            # fetch-and-diff logic here
            return [InboundNotification(...)]

        source = PollingSource(
            name="feed",
            poll_fn=check_feed,
            interval=60.0,  # check every minute
        )
    """

    def __init__(
        self,
        name: str,
        poll_fn: Callable[[], Coroutine[Any, Any, list[InboundNotification]]],
        interval: float = 60.0,
    ) -> None:
        self.name = name
        self._poll_fn = poll_fn
        self._interval = interval
        self._callback: NotificationCallback | None = None
        self._task: asyncio.Task[None] | None = None

    async def start(self, callback: NotificationCallback) -> None:
        self._callback = callback
        self._task = asyncio.create_task(self._poll_loop())
        logger.info(
            "polling_source_started",
            name=self.name,
            interval=self._interval,
        )

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
            self._task = None
        self._callback = None
        logger.info("polling_source_stopped", name=self.name)

    async def _poll_loop(self) -> None:
        """Periodically poll and deliver notifications."""
        while True:
            try:
                notifications = await self._poll_fn()
                if self._callback is not None:
                    for notification in notifications:
                        await self._callback(notification)
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.error(
                    "polling_source_error",
                    name=self.name,
                    error=str(exc),
                )
            await asyncio.sleep(self._interval)
