"""Tests for the dependency-free cron evaluator."""

from __future__ import annotations

from datetime import UTC, datetime
from zoneinfo import ZoneInfo

import pytest

from crewlet.schedule.cron import (
    CronError,
    iter_fire_times,
    next_fire,
    parse_cron,
    prev_fire,
    validate_cron,
)


def _dt(y, mo, d, h, mi, tz=UTC):
    return datetime(y, mo, d, h, mi, tzinfo=tz)


# --- parsing ---------------------------------------------------------------


@pytest.mark.parametrize(
    "expr",
    [
        "0 9 * * 1-5",
        "*/15 * * * *",
        "0 9,17 * * *",
        "0 0 1 * *",
        "0 9 * * MON",
        "30 9 * * 5",
        "0 0 13 * 5",
        "0 0 * jan-mar *",
    ],
)
def test_parse_valid(expr):
    validate_cron(expr)  # does not raise


@pytest.mark.parametrize(
    "expr",
    [
        "",
        "* * * *",  # 4 fields
        "* * * * * *",  # 6 fields
        "60 * * * *",  # minute out of range
        "* 24 * * *",  # hour out of range
        "0 9 * * 8",  # dow out of range (max 7)
        "0 9 32 * *",  # dom out of range
        "5-1 * * * *",  # descending range
        "*/0 * * * *",  # zero step
        "abc * * * *",
    ],
)
def test_parse_invalid(expr):
    with pytest.raises(CronError):
        parse_cron(expr)


# --- matching --------------------------------------------------------------


def test_weekday_nine_am():
    cron = parse_cron("0 9 * * 1-5")
    # 2026-06-08 is a Monday.
    assert cron.matches(_dt(2026, 6, 8, 9, 0))
    assert not cron.matches(_dt(2026, 6, 8, 9, 1))  # wrong minute
    assert not cron.matches(_dt(2026, 6, 8, 8, 0))  # wrong hour
    # 2026-06-13 is a Saturday, 2026-06-14 a Sunday.
    assert not cron.matches(_dt(2026, 6, 13, 9, 0))
    assert not cron.matches(_dt(2026, 6, 14, 9, 0))


def test_step_and_list():
    cron = parse_cron("*/15 * * * *")
    assert cron.matches(_dt(2026, 6, 8, 12, 0))
    assert cron.matches(_dt(2026, 6, 8, 12, 15))
    assert cron.matches(_dt(2026, 6, 8, 12, 45))
    assert not cron.matches(_dt(2026, 6, 8, 12, 7))

    cron2 = parse_cron("0 9,17 * * *")
    assert cron2.matches(_dt(2026, 6, 8, 9, 0))
    assert cron2.matches(_dt(2026, 6, 8, 17, 0))
    assert not cron2.matches(_dt(2026, 6, 8, 13, 0))


def test_dow_names_and_sunday_aliases():
    assert parse_cron("0 9 * * MON") == parse_cron("0 9 * * 1")
    # 7 and 0 both mean Sunday.
    sun7 = parse_cron("0 9 * * 7")
    sun0 = parse_cron("0 9 * * 0")
    assert sun7.dows == sun0.dows
    assert sun0.matches(_dt(2026, 6, 14, 9, 0))  # Sunday


def test_dom_dow_or_semantics():
    # "13th of the month OR any Friday" (both fields restricted → OR).
    cron = parse_cron("0 0 13 * 5")
    assert cron.matches(_dt(2026, 6, 13, 0, 0))  # the 13th (a Saturday)
    assert cron.matches(_dt(2026, 6, 12, 0, 0))  # a Friday, not the 13th
    assert not cron.matches(_dt(2026, 6, 10, 0, 0))  # Wednesday, not 13th


def test_dom_only_restricted():
    cron = parse_cron("0 0 1 * *")
    assert cron.matches(_dt(2026, 6, 1, 0, 0))
    assert not cron.matches(_dt(2026, 6, 2, 0, 0))


# --- windowed iteration + next/prev ---------------------------------------


def test_iter_fire_times_window_utc():
    cron = parse_cron("0 9 * * *")
    fires = iter_fire_times(
        cron,
        after_utc=_dt(2026, 6, 8, 8, 59),
        until_utc=_dt(2026, 6, 8, 9, 0),
        tz=UTC,
    )
    assert fires == [_dt(2026, 6, 8, 9, 0)]


def test_iter_fire_times_exclusive_lower_bound():
    cron = parse_cron("0 9 * * *")
    # Lower bound is exclusive: a fire exactly at ``after_utc`` is skipped.
    fires = iter_fire_times(
        cron,
        after_utc=_dt(2026, 6, 8, 9, 0),
        until_utc=_dt(2026, 6, 8, 9, 5),
        tz=UTC,
    )
    assert fires == []


def test_timezone_conversion():
    # 09:30 in Amsterdam (CEST = UTC+2 in June) is 07:30 UTC.
    cron = parse_cron("30 9 * * *")
    tz = ZoneInfo("Europe/Amsterdam")
    nxt = next_fire(cron, after_utc=_dt(2026, 6, 8, 0, 0), tz=tz)
    assert nxt == _dt(2026, 6, 8, 7, 30)


def test_next_and_prev_fire():
    cron = parse_cron("0 9 * * *")
    after = _dt(2026, 6, 8, 10, 0)
    assert next_fire(cron, after_utc=after, tz=UTC) == _dt(2026, 6, 9, 9, 0)
    assert prev_fire(cron, at_utc=after, tz=UTC) == _dt(2026, 6, 8, 9, 0)
    # prev_fire is inclusive of the boundary minute.
    boundary = _dt(2026, 6, 8, 9, 0)
    assert prev_fire(cron, at_utc=boundary, tz=UTC) == boundary
