"""Dependency-free 5-field cron evaluator (standard Vixie semantics).

Crewlet keeps its runtime dependency set lean, so rather than pull in
``croniter`` we ship a small, fully-tested evaluator for the standard
five-field cron syntax::

    ┌───────────── minute        (0-59)
    │ ┌───────────── hour         (0-23)
    │ │ ┌───────────── day-of-month (1-31)
    │ │ │ ┌───────────── month       (1-12 or JAN-DEC)
    │ │ │ │ ┌───────────── day-of-week (0-7 or SUN-SAT; 0 and 7 = Sunday)
    │ │ │ │ │
    * * * * *

Each field accepts ``*``, single values, ``a-b`` ranges, ``*/step`` and
``a-b/step`` steps, ``a/step`` (= ``a-max/step``), comma-separated lists,
and three-letter month / day names.

Vixie day semantics: when **both** day-of-month and day-of-week are
restricted (i.e. neither is a bare ``*``), a day matches if *either*
field matches.  When only one is restricted, that field alone decides.

Times are matched at minute granularity.  The evaluator works on
timezone-aware datetimes; the :class:`~crewlet.schedule.Scheduler`
iterates instants in UTC and converts each to the schedule's timezone
(``dt.astimezone(tz)``) before calling :meth:`CronExpr.matches`, which
keeps DST handling unambiguous (every UTC instant maps to exactly one
local time).
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta, tzinfo

__all__ = [
    "CronError",
    "CronExpr",
    "iter_fire_times",
    "next_fire",
    "parse_cron",
    "prev_fire",
    "validate_cron",
]


class CronError(ValueError):
    """Raised when a cron expression cannot be parsed."""


_MONTHS = {
    "jan": 1,
    "feb": 2,
    "mar": 3,
    "apr": 4,
    "may": 5,
    "jun": 6,
    "jul": 7,
    "aug": 8,
    "sep": 9,
    "oct": 10,
    "nov": 11,
    "dec": 12,
}

_DOWS = {
    "sun": 0,
    "mon": 1,
    "tue": 2,
    "wed": 3,
    "thu": 4,
    "fri": 5,
    "sat": 6,
}


def _parse_value(token: str, lo: int, hi: int, names: dict[str, int]) -> int:
    """Parse a single value token into an int, honouring name aliases."""
    key = token.strip().lower()
    if key in names:
        return names[key]
    try:
        val = int(key)
    except ValueError as exc:
        raise CronError(f"invalid value {token!r}") from exc
    if not (lo <= val <= hi):
        raise CronError(f"value {val} out of range {lo}-{hi}")
    return val


def _parse_field(
    spec: str, lo: int, hi: int, names: dict[str, int] | None = None
) -> tuple[frozenset[int], bool]:
    """Parse one cron field into ``(allowed_values, restricted)``.

    ``restricted`` is ``True`` when the field is anything other than a
    bare ``*`` — it drives the day-of-month / day-of-week OR rule.
    """
    names = names or {}
    spec = spec.strip()
    if not spec:
        raise CronError("empty field")
    restricted = spec != "*"
    allowed: set[int] = set()
    for term in spec.split(","):
        term = term.strip()
        if not term:
            raise CronError(f"empty term in field {spec!r}")
        step = 1
        if "/" in term:
            base, _, step_str = term.partition("/")
            try:
                step = int(step_str)
            except ValueError as exc:
                raise CronError(f"invalid step {step_str!r}") from exc
            if step <= 0:
                raise CronError(f"step must be positive, got {step}")
        else:
            base = term

        base = base.strip()
        if base == "*":
            start, end = lo, hi
        elif "-" in base[1:]:  # range (skip a leading '-' which is invalid anyway)
            start_str, _, end_str = base.partition("-")
            start = _parse_value(start_str, lo, hi, names)
            end = _parse_value(end_str, lo, hi, names)
            if end < start:
                raise CronError(f"range {base!r} is descending")
        else:
            start = _parse_value(base, lo, hi, names)
            # ``a/step`` means a, a+step, ... up to hi.  A bare ``a`` is
            # just the single value.
            end = hi if "/" in term else start
        allowed.update(range(start, end + 1, step))
    return frozenset(allowed), restricted


@dataclass(frozen=True)
class CronExpr:
    """A parsed 5-field cron expression."""

    minutes: frozenset[int]
    hours: frozenset[int]
    doms: frozenset[int]
    months: frozenset[int]
    dows: frozenset[int]
    dom_restricted: bool
    dow_restricted: bool

    def matches(self, dt: datetime) -> bool:
        """Whether *dt* (a local, minute-aligned datetime) is a fire time."""
        if dt.minute not in self.minutes:
            return False
        if dt.hour not in self.hours:
            return False
        if dt.month not in self.months:
            return False
        dom_ok = dt.day in self.doms
        # Python weekday(): Mon=0..Sun=6.  Cron: Sun=0..Sat=6.
        cron_dow = (dt.weekday() + 1) % 7
        dow_ok = cron_dow in self.dows
        if self.dom_restricted and self.dow_restricted:
            return dom_ok or dow_ok
        if self.dom_restricted:
            return dom_ok
        if self.dow_restricted:
            return dow_ok
        return True


def parse_cron(expr: str) -> CronExpr:
    """Parse a 5-field cron expression, raising :class:`CronError`."""
    if not isinstance(expr, str):
        raise CronError("cron expression must be a string")
    fields = expr.split()
    if len(fields) != 5:
        raise CronError(
            f"expected 5 fields (minute hour dom month dow), got {len(fields)}"
        )
    minute_spec, hour_spec, dom_spec, month_spec, dow_spec = fields
    minutes, _ = _parse_field(minute_spec, 0, 59)
    hours, _ = _parse_field(hour_spec, 0, 23)
    doms, dom_restricted = _parse_field(dom_spec, 1, 31)
    months, _ = _parse_field(month_spec, 1, 12, _MONTHS)
    # Day-of-week allows 0-7; normalise 7 -> 0 (both are Sunday).
    raw_dows, dow_restricted = _parse_field(dow_spec, 0, 7, _DOWS)
    dows = frozenset(0 if d == 7 else d for d in raw_dows)
    return CronExpr(
        minutes=minutes,
        hours=hours,
        doms=doms,
        months=months,
        dows=dows,
        dom_restricted=dom_restricted,
        dow_restricted=dow_restricted,
    )


def validate_cron(expr: str) -> None:
    """Raise :class:`CronError` if *expr* is not a valid cron expression."""
    parse_cron(expr)


def _floor_minute(dt: datetime) -> datetime:
    return dt.replace(second=0, microsecond=0)


def iter_fire_times(
    cron: CronExpr,
    *,
    after_utc: datetime,
    until_utc: datetime,
    tz: tzinfo,
) -> list[datetime]:
    """Return UTC fire times in ``(after_utc, until_utc]`` matching in *tz*.

    Both bounds are timezone-aware UTC.  ``after_utc`` is exclusive,
    ``until_utc`` inclusive.  Each returned datetime is a UTC, minute-
    aligned instant whose local (``tz``) projection matches ``cron``.
    """
    out: list[datetime] = []
    if until_utc <= after_utc:
        return out
    m = _floor_minute(after_utc)
    while m <= after_utc:
        m += timedelta(minutes=1)
    while m <= until_utc:
        if cron.matches(m.astimezone(tz)):
            out.append(m)
        m += timedelta(minutes=1)
    return out


def _scan(
    cron: CronExpr, *, start_utc: datetime, tz: tzinfo, forward: bool, max_days: int
) -> datetime | None:
    step = timedelta(minutes=1) if forward else timedelta(minutes=-1)
    limit = (
        start_utc + timedelta(days=max_days)
        if forward
        else start_utc - timedelta(days=max_days)
    )
    m = start_utc
    while (m <= limit) if forward else (m >= limit):
        if cron.matches(m.astimezone(tz)):
            return m
        m += step
    return None


def next_fire(
    cron: CronExpr, *, after_utc: datetime, tz: tzinfo, max_days: int = 400
) -> datetime | None:
    """Return the first UTC fire time strictly after ``after_utc``."""
    m = _floor_minute(after_utc)
    while m <= after_utc:
        m += timedelta(minutes=1)
    return _scan(cron, start_utc=m, tz=tz, forward=True, max_days=max_days)


def prev_fire(
    cron: CronExpr, *, at_utc: datetime, tz: tzinfo, max_days: int = 400
) -> datetime | None:
    """Return the most recent UTC fire time at or before ``at_utc``."""
    m = _floor_minute(at_utc)
    return _scan(cron, start_utc=m, tz=tz, forward=False, max_days=max_days)
