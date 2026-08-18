"""Seat ownership: which node runs which agent.

See :mod:`crewlet.seat.host` for the placement loop and
:mod:`crewlet.seat.watchdog` for the one thing a stalled process can
still do about itself.
"""

from crewlet.seat.host import (
    SEAT_CLAIM_LIMIT_PER_SWEEP,
    SEAT_HEARTBEAT_INTERVAL_SECONDS,
    SEAT_LEASE_TTL_SECONDS,
    SEAT_SWEEP_INTERVAL_SECONDS,
    SeatHost,
    SweepResult,
)
from crewlet.seat.watchdog import EventLoopWatchdog

__all__ = [
    "SEAT_CLAIM_LIMIT_PER_SWEEP",
    "SEAT_HEARTBEAT_INTERVAL_SECONDS",
    "SEAT_LEASE_TTL_SECONDS",
    "SEAT_SWEEP_INTERVAL_SECONDS",
    "EventLoopWatchdog",
    "SeatHost",
    "SweepResult",
]
