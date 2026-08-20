"""The apply-attempt budget is per REVISION, not per process.

A node that cannot apply a revision retries `MAX_APPLY_ATTEMPTS` times
and then goes STUCK: it sheds, releases every seat, and refuses work.
That is the design, and the runbook's remedy is to activate a fixed
revision.

The remedy did not work. `_apply_attempts` counted attempts for the
life of the process, so once it hit the ceiling on epoch 7 the
condition `self._apply_attempts < MAX_APPLY_ATTEMPTS` was false for
epoch 8, 9 and every epoch after — `_converge_to` was never called
again. The node stayed STUCK until someone restarted it, and nothing
in the logs said the new revision had been skipped.
"""

from __future__ import annotations

import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from uuid import UUID, uuid4

sys.path.insert(0, str(Path(__file__).parent.parent))
from conftest import make_bootstrap, make_engine  # noqa: E402
from crewlet.db.config_plane import MAX_APPLY_ATTEMPTS  # noqa: E402
from crewlet.queue.memory import MemoryEventQueue  # noqa: E402


@dataclass
class _Target:
    epoch: int
    revision_id: UUID


class _Plane:
    """The activation pointer, and nothing else the tick needs."""

    def __init__(self, epoch: int) -> None:
        self.target_epoch = epoch
        self.revision_id = uuid4()

    async def target(self) -> _Target:
        return _Target(self.target_epoch, self.revision_id)

    async def peer_health(self, epoch: int, *, exclude_node: str) -> tuple[int, int]:
        return (0, 0)

    async def record_apply(self, *a: Any, **kw: Any) -> None:
        return None


async def _engine(plane: _Plane):
    engine = await make_engine(
        bootstrap=make_bootstrap(), event_queue=MemoryEventQueue()
    )
    engine._config_plane = lambda: plane  # the engine calls it
    engine._company_config_store = object()
    engine._applied_epoch = plane.target_epoch - 1

    # Converging always fails. The real `_converge_to` catches its own
    # exceptions and simply leaves `_applied_epoch` behind, so the fake
    # models a failure by doing nothing rather than by raising.
    async def _fail(*_a: Any, **_kw: Any) -> None:
        return None

    engine._converge_to = _fail  # type: ignore[method-assign]
    return engine


async def test_a_new_revision_gets_a_fresh_budget() -> None:
    """The runbook's remedy, made to work."""
    plane = _Plane(epoch=7)
    engine = await _engine(plane)
    try:
        for _ in range(MAX_APPLY_ATTEMPTS + 2):
            await engine._reconcile_config_once()
        assert engine._apply_attempts == MAX_APPLY_ATTEMPTS, "budget not exhausted"

        # The operator pushes a fixed revision.
        plane.target_epoch = 8
        attempted: list[int] = []

        async def _ok(target: Any, *_a: Any, **_kw: Any) -> None:
            attempted.append(target.epoch)

        engine._converge_to = _ok  # type: ignore[method-assign]
        await engine._reconcile_config_once()
        assert attempted == [8], (
            "the node never attempted the revision that fixes it, and said "
            "nothing about skipping it — STUCK became terminal until restart"
        )
    finally:
        await engine.stop()


async def test_the_budget_still_bounds_one_revision() -> None:
    """Resetting per epoch must not turn the ceiling into no ceiling:
    a node that cannot apply epoch 7 has to stop hammering it."""
    plane = _Plane(epoch=7)
    engine = await _engine(plane)
    try:
        for _ in range(MAX_APPLY_ATTEMPTS + 5):
            await engine._reconcile_config_once()
        assert engine._apply_attempts == MAX_APPLY_ATTEMPTS
    finally:
        await engine.stop()
