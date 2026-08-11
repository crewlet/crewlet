"""Notification prompts registry — maps event sources to prompt formatters.

Usage::

    from crewlet.notifications.notification_prompts import build_notification_prompt

    description = build_notification_prompt(notification)
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from crewlet.notifications.notification_prompts.base import NotificationPrompt
from crewlet.notifications.notification_prompts.confluence import (
    ConfluenceNotificationPrompt,
)
from crewlet.notifications.notification_prompts.github import GitHubNotificationPrompt
from crewlet.notifications.notification_prompts.gitlab import GitLabNotificationPrompt
from crewlet.notifications.notification_prompts.jira import JiraNotificationPrompt
from crewlet.notifications.notification_prompts.plane import PlaneNotificationPrompt
from crewlet.notifications.notification_prompts.slack import SlackNotificationPrompt

if TYPE_CHECKING:
    from crewlet.notifications.handle import HandleRegistry
    from crewlet.notifications.protocol import InboundNotification

# Registry: source name → prompt instance
_PROMPTS: dict[str, NotificationPrompt] = {
    "jira": JiraNotificationPrompt(),
    "confluence": ConfluenceNotificationPrompt(),
    "slack": SlackNotificationPrompt(),
    "github": GitHubNotificationPrompt(),
    "gitlab": GitLabNotificationPrompt(),
    "plane": PlaneNotificationPrompt(),
}

_FALLBACK = NotificationPrompt()


def build_notification_prompt(
    notification: InboundNotification,
    *,
    handle_registry: HandleRegistry | None = None,
) -> str:
    """Build a prompt for an inbound notification.

    Selects the appropriate prompt based on ``notification.source``,
    falling back to the generic prompt for unknown sources.
    ``handle_registry`` lets source-specific builders rewrite raw
    external IDs (Slack user IDs, Jira account IDs) into friendly
    handles in the rendered prompt.
    """
    prompt = _PROMPTS.get(notification.source, _FALLBACK)
    return prompt.build(notification, handle_registry=handle_registry)


def notification_requires_recon(notification: InboundNotification) -> bool:
    """Whether the notification is a *pointer* rather than the context.

    Companion of :func:`build_notification_prompt`: dispatches to the
    same source-specific prompt and asks
    :meth:`NotificationPrompt.requires_recon`.  ``True`` for webhooks
    that name a thing-that-changed without carrying its content
    (Jira / Confluence page events, GitHub review requests, Slack
    thread replies); ``False`` for notifications whose body is the
    message.  Routed onto the ``ExternalNotification`` event so the
    Plan-phase relevance-filter prefetches can skip their aux-LLM
    call when the trigger is too thin to filter against.
    """
    prompt = _PROMPTS.get(notification.source, _FALLBACK)
    return prompt.requires_recon(notification)


def get_prompt(source: str) -> NotificationPrompt:
    """Return the prompt instance registered for *source*.

    Falls back to the generic prompt for unknown sources — same
    dispatch :func:`build_notification_prompt` and
    :func:`notification_requires_recon` use.  The inbox coalescer
    (:mod:`crewlet.notifications.coalesce`) goes through this to reach
    the per-source ``conversation_key`` / ``digest_entry_body`` hooks.
    """
    return _PROMPTS.get(source, _FALLBACK)


def register_prompt(source: str, prompt: NotificationPrompt) -> None:
    """Register a custom prompt for a source.

    Allows extensions to add prompts for new sources without modifying
    this module.
    """
    _PROMPTS[source] = prompt


__all__ = [
    "NotificationPrompt",
    "ConfluenceNotificationPrompt",
    "GitHubNotificationPrompt",
    "GitLabNotificationPrompt",
    "JiraNotificationPrompt",
    "PlaneNotificationPrompt",
    "SlackNotificationPrompt",
    "build_notification_prompt",
    "get_prompt",
    "notification_requires_recon",
    "register_prompt",
]
