"""Role/unit-scoped scheduled work — the cron analogue for agents.

See ``docs/concepts/scheduling.md`` for the design.  Schedules are
declared in the YAML org config on ``Role.schedules`` / ``OrgUnit.schedules``
(modelled by :class:`crewlet.org.models.Schedule`); the :class:`Scheduler`
worker dispatches them.
"""

from crewlet.schedule.cron import (
    CronError,
    CronExpr,
    parse_cron,
    validate_cron,
)
from crewlet.schedule.scheduler import (
    Scheduler,
    count_schedules,
    describe_schedules,
    has_schedules,
    resolve_runners,
)
from crewlet.schedule.store import (
    MemoryScheduledRunStore,
    ScheduledRunStore,
    ScheduledRunStoreProtocol,
)

__all__ = [
    "CronError",
    "CronExpr",
    "MemoryScheduledRunStore",
    "ScheduledRunStore",
    "ScheduledRunStoreProtocol",
    "Scheduler",
    "count_schedules",
    "describe_schedules",
    "has_schedules",
    "parse_cron",
    "resolve_runners",
    "validate_cron",
]
