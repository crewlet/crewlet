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


# ── DST, both directions ─────────────────────────────────────────────
#
# The evaluator iterates UTC and matches the LOCAL projection, which is
# what makes DST unambiguous — every UTC instant has exactly one local
# time. The two consequences are opposite and both are documented in
# docs/concepts/scheduling.md; only one of them was pinned by a test.


def test_a_repeated_local_hour_yields_two_instants():
    """Fall-back: the local minute happens twice, so the evaluator
    reports both UTC instants.

    Collapsing them is the LEDGER's job, not the evaluator's — the fire
    label is the local wall-clock stamp, so both share one identity and
    the schedule fires once. That split is deliberate, and this pins the
    evaluator's half of it: a change here that silently dropped one
    instant would move DST correctness into a place no test covers.
    """
    tz = ZoneInfo("Europe/London")  # 2026-10-25: 02:00 BST -> 01:00 GMT
    cron = parse_cron("30 1 * * *")
    fires = iter_fire_times(
        cron,
        after_utc=_dt(2026, 10, 25, 0, 0),
        until_utc=_dt(2026, 10, 25, 12, 0),
        tz=tz,
    )
    assert fires == [_dt(2026, 10, 25, 0, 30), _dt(2026, 10, 25, 1, 30)]
    # Both are 01:30 local — one wall-clock minute, two instants.
    assert {f.astimezone(tz).strftime("%H%M") for f in fires} == {"0130"}


def test_a_skipped_local_hour_does_not_fire_that_day():
    """Spring-forward: the local minute does not exist, so nothing
    matches and the schedule silently misses that day.

    Documented, and the reason the docs tell you to check WHICH hour
    vanishes in your zone rather than assuming 02:00 — in Europe/London
    it is 01:00-01:59, so this very ordinary-looking daily schedule is
    the one that skips.
    """
    tz = ZoneInfo("Europe/London")  # 2027-03-28: 01:00 GMT -> 02:00 BST
    cron = parse_cron("30 1 * * *")
    fires = iter_fire_times(
        cron,
        after_utc=_dt(2027, 3, 26, 12, 0),
        until_utc=_dt(2027, 3, 30, 12, 0),
        tz=tz,
    )
    days = sorted({f.astimezone(tz).date().isoformat() for f in fires})
    assert days == ["2027-03-27", "2027-03-29", "2027-03-30"], (
        "the skipped-hour day should have no fire at all"
    )
    assert "2027-03-28" not in days


def test_an_hour_outside_the_gap_still_fires_on_the_transition_day():
    """The skip is confined to the vanished hour — the rest of that day
    is ordinary."""
    tz = ZoneInfo("Europe/London")
    cron = parse_cron("30 5 * * *")
    fires = iter_fire_times(
        cron,
        after_utc=_dt(2027, 3, 26, 12, 0),
        until_utc=_dt(2027, 3, 30, 12, 0),
        tz=tz,
    )
    days = sorted({f.astimezone(tz).date().isoformat() for f in fires})
    assert "2027-03-28" in days, "only the vanished hour should be skipped"
    assert days == ["2027-03-27", "2027-03-28", "2027-03-29", "2027-03-30"]
