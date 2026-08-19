"""A two-node fleet, over the memory twin and a real broker alike.

The exit criteria for seat ownership are all statements about **two
nodes**, and nothing else in the suite can make them: a single-process
test can assert that a seat is claimed, attached and torn down in the
right order, but not that the seat's mail follows it, that a node which
lost the seat refuses to work it, or that a completion lands on the one
node that can act on it.

So this fixture builds a real fleet: two :class:`~crewlet.engine.Engine`
objects, each with its own node id, its own lease incarnation and its own
queue client, sharing one placement table and one broker.  Everything
under test — placement, attachment, admission, routing — is the
production code path.  Two things are stubbed and both are deliberate:

* ``turn_engine.run_turn`` becomes a recorder.  What a turn *does* is
  not the subject; **which node ran it, for which seat, in what order**
  is, and that is exactly what the recorder captures.  It also keeps the
  promise that tests never call a real LLM.
* the sandbox provider is ``type: fake``.  The routing property is that
  a completion reaches the seat's owner and only the seat's owner, which
  is decided by who is attached to the control topic — no box required.

The whole suite is parametrized over the queue backend.  The memory twin
is not a mock of the broker here, it is a second implementation of it,
and running the same criteria against both is what stops CI certifying a
behaviour Pulsar does not have.  Skipping is not passing: without a
broker on ``localhost:6650`` the Pulsar parameter skips, and the run is
only as strong as the twin.
"""

from __future__ import annotations

import asyncio
import contextlib
import socket
import sys
from collections.abc import Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from uuid import uuid4

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from conftest import make_bootstrap  # noqa: E402
from crewlet._tasks import cancel_and_wait  # noqa: E402
from crewlet.config import (  # noqa: E402
    CompanyConfig,
    LLMProviderConfig,
    ProvidersConfig,
    SandboxProviderConfig,
)
from crewlet.db.leases import MemoryLeaseStore  # noqa: E402
from crewlet.db.turn_completions import (  # noqa: E402
    MemoryTurnCompletionStore,
)
from crewlet.engine import Engine  # noqa: E402
from crewlet.events.types import Event  # noqa: E402
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402

PULSAR_URL = "pulsar://localhost:6650"


def _pulsar_available() -> bool:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.settimeout(1)
    try:
        sock.connect(("localhost", 6650))
        return True
    except OSError:
        return False
    finally:
        sock.close()


_PULSAR_UP = _pulsar_available()

BACKENDS = [
    pytest.param("memory", id="memory"),
    pytest.param(
        "pulsar",
        id="pulsar-real",
        marks=[
            pytest.mark.integration,
            pytest.mark.skipif(
                not _PULSAR_UP, reason="no Pulsar broker at localhost:6650"
            ),
        ],
    ),
]

# How long a criterion waits for a broker round-trip before failing.
#
# Generous on purpose: the memory twin dispatches inside ``publish`` and
# never spends any of it, so this budget is only ever paid by the real
# broker, where an attach is ~5 ms and a redelivery after a close is
# ~9 ms (see tests/test_queue/test_broker_behavior.py).  Five seconds is
# two orders of magnitude of headroom on a loaded CI box, and a test that
# actually exhausts it has found something rather than been unlucky.
SETTLE_TIMEOUT = 5.0


async def settle(predicate, *, timeout: float = SETTLE_TIMEOUT, what: str = "") -> None:
    """Wait for ``predicate()`` to hold, or fail saying what did not."""
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        if predicate():
            return
        await asyncio.sleep(0.02)
    raise AssertionError(f"timed out after {timeout}s waiting for {what or predicate}")


@dataclass
class _Node:
    """One engine in the fleet, plus what it was observed doing."""

    name: str
    engine: Engine
    queue: Any
    journal: list[tuple[str, str, str]] = field(default_factory=list)
    """Shared, fleet-wide ``(node, seat handle, marker)`` in the order
    turns actually ran.

    Per-node lists cannot answer the ordering criterion: reading them one
    after the other reports every turn node A ran before every turn node
    B ran, which for a seat that moved mid-conversation is exactly the
    wrong order. One append-ordered log across the fleet is the only
    honest record."""
    control: list[tuple[str, str]] = field(default_factory=list)
    """``(seat handle, turn id)`` per control event this node received."""
    dead: bool = False
    """Set by :meth:`kill`; teardown skips a graceful stop for these."""

    @property
    def turns(self) -> list[tuple[str, str]]:
        return [(h, m) for node, h, m in self.journal if node == self.name]

    @property
    def held(self) -> tuple[str, ...]:
        return self.engine._seat_host.held_handles

    def markers(self, handle: str) -> list[str]:
        return [m for h, m in self.turns if h == handle]

    def attached_to(self, topic: str, group: str) -> bool:
        return (topic, group) in self.queue.attachments()

    async def sweep(self) -> Any:
        return await self.engine._seat_host.sweep()

    async def kill(self, *, in_flight: Sequence[asyncio.Task[Any]] = ()) -> None:
        """Make this node VANISH, the way ``kill -9`` does.

        Not ``stop()``. A graceful stop drains, releases every lease and
        detaches every consumer, which is the one shutdown path that
        cannot exercise takeover — nothing is left for a peer to take
        over FROM. What a killed process leaves behind is the opposite:

        - **leases still held, and no longer renewed.** They lapse at the
          TTL, and until they do no peer may touch the seat. This is what
          bounds takeover latency, and the only thing that makes the
          window observable.
        - **in-flight deliveries unacked.** The handler never returned,
          so the broker eventually hands the message to whoever attaches
          next. Modelled by CANCELLING the tasks running them: the queue
          puts a cancelled delivery back at the front of its backlog,
          which is what the Pulsar backend achieves with a NAK.
        - **no detach handshake.** The consumer is simply gone.

        ``in_flight`` is the memory backend's share of this: that twin
        dispatches INSIDE ``publish``, so the task running a handler is
        the publisher's, and only the caller knows which one that is. On
        Pulsar the consume loops are the queue's own and are cancelled
        here.
        """
        host = self.engine._seat_host
        host._running = False
        for task in list(host._tasks):
            await cancel_and_wait(task)
        host._tasks.clear()

        for task in in_flight:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await task

        for sub in list(getattr(self.queue, "_subscriptions", [])):
            task = getattr(sub, "task", None)
            if task is not None:
                await cancel_and_wait(task)

        for topic, group in list(self.queue.attachments()):
            with contextlib.suppress(Exception):
                await self.queue.detach(topic, group)
        self.dead = True


@dataclass
class Fleet:
    """Two nodes, one company, one placement table, one broker."""

    nodes: list[_Node]
    handles: tuple[str, ...]
    leases: MemoryLeaseStore
    completions: MemoryTurnCompletionStore
    company: CompanyConfig
    _published: dict[str, list[Any]] = field(default_factory=dict)

    @property
    def a(self) -> _Node:
        return self.nodes[0]

    @property
    def b(self) -> _Node:
        return self.nodes[1]

    def owner_of(self, handle: str) -> _Node:
        owners = [n for n in self.nodes if handle in n.held]
        assert len(owners) == 1, f"seat {handle!r} held by {[n.name for n in owners]}"
        return owners[0]

    def other_than(self, node: _Node) -> _Node:
        return next(n for n in self.nodes if n is not node)

    def movable_seat(self) -> tuple[str, _Node, _Node]:
        """``(handle, its owner, a node with room to take it)``.

        Staging a handoff needs a successor that can actually claim, and
        a node already at its fair share cannot — so the seat has to come
        off the fuller node. Deterministic given the layout, so a failure
        here is a placement failure rather than a coin toss.
        """
        fuller, leaner = sorted(self.nodes, key=lambda n: len(n.held), reverse=True)
        assert len(fuller.held) > len(leaner.held), (
            f"no node has room: {[(n.name, n.held) for n in self.nodes]}"
        )
        return sorted(fuller.held)[0], fuller, leaner

    async def balance(self) -> None:
        """Sweep until placement converges on the fair share.

        The fixture starts one node at a time, so the first holds every
        seat for as long as it is alone. Converging is the give-back plus
        a claim — two sweeps, in that order.
        """
        for node in self.nodes:
            await node.sweep()
        for node in self.nodes:
            await node.sweep()

    async def publish(self, handle: str, marker: str) -> None:
        """Send one uniquely identifiable trigger to a seat's inbox.

        ``task_assigned`` on purpose: it keys uniquely, so the inbox
        batcher delivers each one alone and the recorder sees an order
        rather than a set.
        """
        from crewlet.queue.topics import agent_inbox_topic

        event = Event(
            type="task_assigned",
            source="fleet-test",
            payload={"task_id": marker, "task_description": marker},
        )
        self._published.setdefault(handle, []).append(event)
        await self.nodes[0].queue.publish(agent_inbox_topic(handle), event)

    def published_ids(self, handle: str) -> list[Any]:
        """Every inbox event this fleet published to ``handle``.

        Kept so a test can redeliver the SAME event — identity intact —
        which is what a broker does with an unacked message and the only
        way to exercise a ledger keyed on it.
        """
        return list(self._published.get(handle, []))

    def turns_for(self, handle: str) -> list[tuple[str, str]]:
        """``(node name, marker)`` for every turn on a seat, in real order."""
        return [(node, m) for node, h, m in self.nodes[0].journal if h == handle]


def _company(suffix: str, handles: tuple[str, ...]) -> CompanyConfig:
    return CompanyConfig(
        name=f"Fleet {suffix}",
        mission="prove a seat is not a thing you can half-own",
        # THREE seats over two nodes, not two. With one seat each,
        # every node sits exactly at ``ceil(2/2) == 1`` and a handoff is
        # impossible to stage: the only node that could take a released
        # seat is the node that just released it. Three makes the share
        # two, so there is always a node with room for one more — which
        # is also the ordinary shape of a real company, where seats do
        # not divide evenly by nodes.
        roles=[
            {"name": "Alpha", "handle": handles[0], "goal": "lead"},
            {"name": "Beta", "handle": handles[1], "goal": "build"},
            {"name": "Gamma", "handle": handles[2], "goal": "review"},
        ],
        providers=ProvidersConfig(
            llm={"default": LLMProviderConfig(model="gpt-4o-mini")},
            sandbox=SandboxProviderConfig(type="fake"),
        ),
    )


def _instrument(node: _Node) -> None:
    """Replace the two bodies whose *callers* are the subject.

    ``run_turn`` and the coordinator's control adapter are the last hop
    in each path under test. Recording at that hop leaves everything
    before it — placement, attachment, dedupe, the ownership branch,
    routing — running as it does in production.
    """
    engine = node.engine

    async def _record_turn(agent: Any, **kwargs: Any) -> str:
        event = kwargs.get("event")
        marker = ""
        if event is not None:
            marker = str(event.payload.get("task_id", event.type))
        node.journal.append((node.name, agent.handle, marker))
        return "recorded"

    assert engine.turn_engine is not None, "the fleet needs a turn engine to gate"
    engine.turn_engine.run_turn = _record_turn  # type: ignore[method-assign]

    coordinator = engine._sandbox_coordinator
    assert coordinator is not None, "the fleet needs a sandbox coordinator to gate"

    # Wrapped on the per-event methods, NOT on ``_on_control_event``: the
    # seat's control subscription was created during ``start`` and holds
    # a reference to the bound adapter, so replacing that attribute now
    # would leave the queue calling the original. ``_on_control_event``
    # looks these two up on the instance at call time, so wrapping them
    # is seen by the live subscription.
    def _recorder(name: str) -> Any:
        inner = getattr(coordinator, name)

        async def _record(event: Any) -> None:
            node.control.append(
                (
                    str(getattr(event, "agent_handle", "")),
                    str(getattr(event, "turn_id", "")),
                )
            )
            await inner(event)

        return _record

    for method in ("on_run_started", "on_run_completed"):
        setattr(coordinator, method, _recorder(method))


@pytest.fixture(params=BACKENDS)
async def fleet(request: pytest.FixtureRequest, monkeypatch: pytest.MonkeyPatch):
    backend = request.param
    # Constructed, never called: ``run_turn`` is recorded below, so no
    # request ever leaves the process. The key has to exist for the
    # provider object to build at all.
    monkeypatch.setenv("OPENAI_API_KEY", "test-key-for-construction")

    suffix = uuid4().hex[:8]
    # Unique handles per fleet, because the topic name is derived from
    # the handle: on a real broker two runs of this suite would otherwise
    # share a seat's subscription and inherit each other's backlog.
    handles = (f"alpha{suffix}", f"beta{suffix}", f"gamma{suffix}")
    company = _company(suffix, handles)
    leases = MemoryLeaseStore()

    # One broker, one client per node — the shape the real deployment
    # has, and the shape the criteria are written against. A single
    # shared queue OBJECT would make every node's attachment look like
    # every other node's, which is exactly the state the phase exists to
    # make impossible.
    seed = MemoryEventQueue() if backend == "memory" else None
    # ONE ledger for the fleet, like the lease table. A per-node ledger
    # deduplicates nothing across a takeover, which is the only thing it
    # is for — and would look from the outside exactly as though it did.
    completions = MemoryTurnCompletionStore()
    journal: list[tuple[str, str, str]] = []
    nodes: list[_Node] = []
    for name in ("node-a", "node-b"):
        if seed is not None:
            queue: Any = seed.client()
        else:
            from crewlet.queue.pulsar import PulsarEventQueue

            queue = PulsarEventQueue(PULSAR_URL)
        bootstrap = make_bootstrap()
        bootstrap.node.id = f"{name}-{suffix}"
        engine = Engine.from_bootstrap(
            bootstrap,
            event_queue=queue,
            lease_store=leases,
            turn_completion_store=completions,
        )
        await engine.apply_config(company)
        await engine.start()
        node = _Node(name=name, engine=engine, queue=queue, journal=journal)
        _instrument(node)
        nodes.append(node)

    built = Fleet(
        nodes=nodes,
        handles=handles,
        leases=leases,
        completions=completions,
        company=company,
    )
    try:
        yield built
    finally:
        for node in reversed(nodes):
            try:
                await node.engine.stop()
            except Exception:  # pragma: no cover — teardown must not mask
                await node.engine._force_stop()
