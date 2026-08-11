"""TimescaleDB event store — persistent storage for all Crewlet events.

Backed by a TimescaleDB hypertable (``crewlet_events``) in the same
PostgreSQL instance that holds the operational tables (``token_usage``,
``agent_diary``, ``synthesized_skills``, ...).  One database for
everything.
"""

from crewlet.timescaledb.buffer import BufferedEventStore
from crewlet.timescaledb.composite import CompositeEventStore
from crewlet.timescaledb.memory import MemoryEventStore
from crewlet.timescaledb.protocol import EventStore
from crewlet.timescaledb.repository import TimescaleDBEventStore
from crewlet.timescaledb.writer import EventStoreWriter

__all__ = [
    "BufferedEventStore",
    "CompositeEventStore",
    "EventStore",
    "EventStoreWriter",
    "MemoryEventStore",
    "TimescaleDBEventStore",
]
