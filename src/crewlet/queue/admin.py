"""Broker administration — creating and deleting subscriptions.

A seat's inbox subscription must exist **whether or not any node owns the
seat**.  That is what makes an unowned seat safe: a durable subscription
retains its backlog with no consumer attached, so a publish that lands
during a lease gap, a claim ramp, or a full fleet restart is held rather
than discarded.  Without the invariant the first publish to a
never-subscribed seat is dropped on the floor — no dead letter, no
producer error, nothing to alert on.

The obvious way to establish it is to subscribe and immediately close.
That is unsafe, and measured to be so
(``tests/test_queue/test_broker_behavior.py``): a seat's subscription is
**Shared**, so a second consumer joining one an owner is actively serving
takes a share of that seat's live traffic into its own prefetch — 12 of
20 messages in the measurement.  A node doing that for every seat at boot
manufactures exactly the double-consumer state seat ownership exists to
prevent.

So subscription lifecycle goes through the broker's admin API instead,
where it needs no consumer at all:

- ``ensure_subscription`` creates the subscription **at the earliest
  message**.  Not a detail: a subscription created at ``Latest`` would
  exist and still lose everything published before its first consumer
  attached, which is the failure the invariant exists to prevent.
- ``delete_subscription`` needs no local consumer either, so
  decommissioning a role does not depend on which node happened to run
  it.  ``Consumer.unsubscribe()`` — the only client-side route — does.

Both are idempotent by tolerance rather than by the broker: creating an
existing subscription answers 409 and deleting an absent one answers 404,
and both are the desired end state, so each returns a bool saying whether
*this* call changed anything rather than raising.

The engine previously never spoke to the admin API at all.  It does now,
which makes the admin endpoint a deployment requirement — see
``QueueConfig.admin_url`` and ``docs/guides/deployment.md``.
"""

from __future__ import annotations

from typing import Protocol
from urllib.parse import quote, urlsplit

import httpx

from crewlet._logging import get_logger

logger = get_logger("queue.admin")

#: Admin ports by broker scheme, per Pulsar's own defaults.  Used to
#: derive an admin URL when the operator has not configured one.
_ADMIN_PORTS = {"pulsar": ("http", 8080), "pulsar+ssl": ("https", 8443)}

#: Seconds to wait on an admin call.  These run at boot, at seat
#: decommission, and behind a singleton lease — never on a delivery hot
#: path — so the budget is sized for a broker under load rather than for
#: latency.  Ten seconds is Pulsar's own client default for admin calls.
_ADMIN_TIMEOUT_SECONDS = 10.0


class BrokerAdminError(RuntimeError):
    """An admin call failed in a way the caller must not treat as done.

    Distinct from the tolerated already-exists / already-gone answers,
    which are successes.  A caller that swallowed this would report the
    subscription-existence invariant established when it is not.
    """


class BrokerAdmin(Protocol):
    """Subscription lifecycle, independent of any consumer."""

    async def subscriptions(self, subject: str) -> list[str]:
        """Subscription names on ``subject``; ``[]`` when it has none.

        A topic that does not exist has no subscriptions — that is an
        answer, not an error.
        """
        ...

    async def ensure_subscription(self, subject: str, group: str) -> bool:
        """Create ``group`` on ``subject`` at the earliest message.

        Returns whether this call created it.  Creating the topic as a
        side effect is intended: a brand-new company's seats have never
        been published to.
        """
        ...

    async def delete_subscription(self, subject: str, group: str) -> bool:
        """Delete ``group`` from ``subject``.

        Returns whether this call deleted it — ``False`` means it was
        already gone, which is the desired end state.
        """
        ...

    async def close(self) -> None:
        """Release the admin client's resources."""
        ...


def derive_admin_url(broker_url: str) -> str:
    """The admin endpoint implied by a broker service URL.

    ``pulsar://host:6650`` → ``http://host:8080``;
    ``pulsar+ssl://host:6651`` → ``https://host:8443``.  Pulsar's own
    default ports, and the layout every standalone and helm deployment
    ships, so an operator who has not thought about the admin endpoint
    still gets a working one.

    Raises ``ValueError`` for a URL this cannot be derived from — better
    than guessing an endpoint that answers someone else's API.
    """
    parts = urlsplit(broker_url)
    mapping = _ADMIN_PORTS.get(parts.scheme)
    if mapping is None or not parts.hostname:
        raise ValueError(
            f"cannot derive an admin URL from {broker_url!r} — set "
            f"providers.queue.admin_url explicitly"
        )
    scheme, port = mapping
    host = f"[{parts.hostname}]" if ":" in parts.hostname else parts.hostname
    return f"{scheme}://{host}:{port}"


class PulsarBrokerAdmin:
    """:class:`BrokerAdmin` over Pulsar's admin v2 REST API."""

    def __init__(
        self,
        admin_url: str,
        *,
        tenant: str = "public",
        namespace: str = "default",
        auth_token: str = "",
        tls_trust_certs_path: str = "",
    ) -> None:
        self._base = admin_url.rstrip("/")
        self._ns_path = f"{tenant}/{namespace}"
        headers = {"Authorization": f"Bearer {auth_token}"} if auth_token else {}
        # The same JWT the broker connection presents: the admin API is
        # a second door onto the same authorization, not an unguarded
        # one.  A token scoped to produce+consume on this namespace is
        # NOT sufficient for these calls — see the deployment guide.
        self._client = httpx.AsyncClient(
            headers=headers,
            timeout=_ADMIN_TIMEOUT_SECONDS,
            verify=tls_trust_certs_path or True,
        )

    def _topic_path(self, subject: str) -> str:
        return f"{self._base}/admin/v2/persistent/{self._ns_path}/{quote(subject)}"

    def _sub_path(self, subject: str, group: str) -> str:
        return f"{self._topic_path(subject)}/subscription/{quote(group)}"

    async def _request(self, method: str, url: str, **kw: object) -> httpx.Response:
        try:
            return await self._client.request(method, url, **kw)  # type: ignore[arg-type]
        except httpx.HTTPError as exc:
            raise BrokerAdminError(
                f"{method} {url} failed: {exc}. The engine needs the broker's "
                f"admin endpoint to keep every seat's subscription alive; see "
                f"providers.queue.admin_url"
            ) from exc

    async def subscriptions(self, subject: str) -> list[str]:
        resp = await self._request("GET", f"{self._topic_path(subject)}/subscriptions")
        if resp.status_code == 404:
            # No topic yet, therefore no subscriptions.  A seat that has
            # never been published to is the normal state of a new
            # company, not a failure.
            return []
        if resp.status_code != 200:
            raise BrokerAdminError(
                f"listing subscriptions for {subject!r} returned "
                f"{resp.status_code}: {resp.text[:200]}"
            )
        names = resp.json()
        return [str(n) for n in names] if isinstance(names, list) else []

    async def ensure_subscription(self, subject: str, group: str) -> bool:
        resp = await self._request(
            "PUT",
            self._sub_path(subject, group),
            # ``"earliest"``, always.  A subscription created at the
            # latest message would exist and still discard everything
            # published before its first consumer attached.
            content=b'"earliest"',
            headers={"Content-Type": "application/json"},
        )
        if resp.status_code == 409:
            return False  # already exists — the end state we wanted
        if resp.status_code not in (200, 204):
            raise BrokerAdminError(
                f"creating subscription {group!r} on {subject!r} returned "
                f"{resp.status_code}: {resp.text[:200]}"
            )
        logger.info("subscription_created", subject=subject, group=group)
        return True

    async def delete_subscription(self, subject: str, group: str) -> bool:
        resp = await self._request("DELETE", self._sub_path(subject, group))
        if resp.status_code == 404:
            return False  # already gone — the end state we wanted
        if resp.status_code not in (200, 204):
            raise BrokerAdminError(
                f"deleting subscription {group!r} on {subject!r} returned "
                f"{resp.status_code}: {resp.text[:200]}"
            )
        logger.info("subscription_deleted", subject=subject, group=group)
        return True

    async def close(self) -> None:
        await self._client.aclose()
