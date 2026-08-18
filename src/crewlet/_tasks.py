"""Small asyncio helpers with one job each.

``cancel_and_wait`` exists because the obvious idiom for "stop this
background task" is subtly wrong, and was written that way in a dozen
places::

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

That swallows two different cancellations. The one it means to swallow is
the task's own, raised back out of the ``await`` because the task ended
cancelled. The one it must NOT swallow is the **caller's**: if the
coroutine doing the teardown is itself cancelled while parked on that
``await``, the ``CancelledError`` delivered to it looks identical and is
suppressed too. The teardown then continues as though nothing happened,
and the shutdown that asked it to stop is silently absorbed.

It matters most exactly where it is most used — a lease release racing a
process shutdown, a queue unsubscribe racing ``stop()`` — because those
are the paths where both cancellations are live at once.
"""

from __future__ import annotations

import asyncio
from typing import Any


async def cancel_and_wait(task: asyncio.Task[Any] | None) -> None:
    """Cancel ``task`` and wait for it to finish, without stealing our own
    cancellation.

    ``asyncio.wait`` is the whole trick: it reports completion instead of
    re-raising what the task ended with, so the task's own
    ``CancelledError`` never reaches us — while a cancellation aimed at
    *this* coroutine still propagates out of the ``await`` as it should.
    """
    if task is None:
        return
    task.cancel()
    await asyncio.wait({task})
