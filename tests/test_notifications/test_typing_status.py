"""Tests for the Slack working-status ("is thinking…") driver."""

from __future__ import annotations

import asyncio

import pytest

from crewlet.notifications.typing_status import (
    DEFAULT_PHASE,
    PHASE_PHRASES,
    ChatConversation,
    StatusPhrases,
    WorkingStatusDriver,
    WorkingStatusMode,
    agent_is_addressed,
    chat_conversation,
)


class FakePoster:
    """Records every ``assistant.threads.setStatus`` call."""

    status_backend = "slack"
    supports_status_text = True
    status_refresh_interval = 45.0
    dm_channel_id_prefix = "D"

    def __init__(self, ok: bool = True) -> None:
        self.ok = ok
        self.calls: list[tuple[str, str, str, str]] = []
        self.raises = False

    async def set_status(
        self, handle: str, channel: str, thread_ts: str, status: str
    ) -> bool:
        if self.raises:
            raise RuntimeError("slack exploded")
        self.calls.append((handle, channel, thread_ts, status))
        return self.ok

    @property
    def statuses(self) -> list[str]:
        return [c[3] for c in self.calls]


SLACK_DM = {
    "transport": "slack",
    "channel": "D123",
    "channel_type": "im",
    "ts": "1700000000.000100",
    "thread_ts": "",
}

SLACK_MENTION = {
    "transport": "slack",
    "channel": "C999",
    "channel_type": "channel",
    "ts": "1700000000.000200",
    "thread_ts": "",
    "thread_follow_reason": "mention",
}

SLACK_PASSIVE = {
    "transport": "slack",
    "channel": "C999",
    "channel_type": "channel",
    "ts": "1700000000.000300",
    "thread_ts": "",
}


# --- conversation resolution ---------------------------------------------


def test_conversation_uses_thread_ts_when_present():
    conv = chat_conversation(
        {
            "transport": "slack",
            "channel": "C1",
            "ts": "2.0",
            "thread_ts": "1.0",
        },
        "slack",
    )
    assert conv == ChatConversation(channel="C1", thread_id="1.0")


def test_conversation_falls_back_to_message_ts_for_top_level():
    """A top-level message has no thread yet — its own ts anchors the
    thread the agent will reply under."""
    conv = chat_conversation(
        {"transport": "slack", "channel": "C1", "ts": "2.0"}, "slack"
    )
    assert conv == ChatConversation(channel="C1", thread_id="2.0")


@pytest.mark.parametrize(
    "metadata",
    [
        None,
        {},
        {"transport": "jira", "channel": "C1", "ts": "1.0"},
        {"transport": "slack", "ts": "1.0"},  # no channel
        {"transport": "slack", "channel": "C1"},  # no anchor
    ],
)
def test_conversation_none_for_non_slack_or_incomplete(metadata):
    assert chat_conversation(metadata, "slack") is None


def test_conversation_coerces_non_string_metadata():
    conv = chat_conversation({"transport": "slack", "channel": "C1", "ts": 2}, "slack")
    assert conv == ChatConversation(channel="C1", thread_id="2")


# --- addressed predicate --------------------------------------------------


@pytest.mark.parametrize(
    "metadata",
    [
        SLACK_DM,
        {"transport": "slack", "channel": "D77", "ts": "1.0"},  # D-prefix, no type
        {"transport": "slack", "channel": "C1", "channel_type": "mpim", "ts": "1.0"},
        SLACK_MENTION,
        {
            "transport": "slack",
            "channel": "C1",
            "ts": "1.0",
            "thread_following": "true",
        },
    ],
)
def test_addressed_true(metadata):
    assert agent_is_addressed(metadata, dm_channel_id_prefix="D") is True


def test_dm_channel_prefix_is_opt_in_per_backend():
    """The ``D``-prefix shortcut is Slack's, and must not leak to backends
    whose channel ids are opaque.

    A Mattermost channel id is a 26-char alphanumeric that can begin with
    ``D`` by chance; treating that as a DM would raise a working
    indicator on ordinary public-channel traffic nobody addressed to this
    agent.  Without an explicit prefix the check is metadata-only.
    """
    mattermost_public = {
        "transport": "mattermost",
        "channel": "Dq1kw8s7z3bo9dxaqmc1yr6rpe",
        "channel_type": "O",
        "ts": "1.0",
    }
    assert agent_is_addressed(mattermost_public) is False
    # ...and a real Mattermost DM still resolves, via channel_type.
    assert agent_is_addressed({**mattermost_public, "channel_type": "D"}) is True


@pytest.mark.parametrize(
    "metadata",
    [
        SLACK_PASSIVE,
        # A collective address (@here / @channel) wakes every bot in the
        # channel and the triage prompt tells most to stay silent — it is
        # deliberately NOT treated as addressing this agent.
        {
            "transport": "slack",
            "channel": "C1",
            "ts": "1.0",
            "thread_follow_reason": "collective",
        },
    ],
)
def test_addressed_false(metadata):
    assert agent_is_addressed(metadata) is False


# --- phrase pools ---------------------------------------------------------


def test_every_pool_completes_the_slack_sentence():
    """Slack renders "*<agent> <status>*", so every phrase must read as a
    continuation of the agent's name."""
    for phase, pool in PHASE_PHRASES.items():
        assert pool, f"{phase} pool is empty"
        assert len(set(pool)) == len(pool), f"{phase} pool repeats a phrase"
        for phrase in pool:
            assert phrase.startswith("is "), (phase, phrase)
            assert phrase.endswith("..."), (phase, phrase)


def test_pick_is_deterministic_and_stays_in_the_pool():
    phrases = StatusPhrases()
    picked = phrases.pick("plan", seed="swe|C1|1.0|t1", rotation=0)
    assert picked in PHASE_PHRASES["plan"]
    assert picked == phrases.pick("plan", seed="swe|C1|1.0|t1", rotation=0)


def test_rotation_walks_the_whole_pool():
    """Successive rotations never repeat until the pool is exhausted —
    that is what makes a phase change visible to the reader."""
    phrases = StatusPhrases()
    pool = PHASE_PHRASES["plan"]
    walked = {phrases.pick("plan", seed="s", rotation=i) for i in range(len(pool))}
    assert walked == set(pool)


def test_unknown_phase_draws_from_the_default_pool():
    assert (
        StatusPhrases().pick("nonsense", seed="s", rotation=0)
        in PHASE_PHRASES[DEFAULT_PHASE]
    )


def test_overrides_replace_only_the_named_phases():
    phrases = StatusPhrases.with_overrides({"plan": ["is nimbusing..."]})
    assert phrases.pick("plan", seed="s", rotation=0) == "is nimbusing..."
    assert phrases.pick("execute", seed="s", rotation=0) in PHASE_PHRASES["execute"]


def test_empty_override_keeps_the_builtin_pool():
    phrases = StatusPhrases.with_overrides({"plan": []})
    assert phrases.pick("plan", seed="s", rotation=0) in PHASE_PHRASES["plan"]
    assert StatusPhrases.with_overrides(None).pools == PHASE_PHRASES


def test_overrides_are_whitespace_trimmed():
    phrases = StatusPhrases.with_overrides({"plan": ["  is crewing...  "]})
    assert phrases.pick("plan", seed="s", rotation=0) == "is crewing..."


# --- session lifecycle ----------------------------------------------------


async def test_begin_sets_status_and_end_clears_it():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)

    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    assert len(poster.calls) == 1
    assert poster.calls[0][:3] == ("swe", "D123", "1700000000.000100")
    assert poster.calls[0][3] in PHASE_PHRASES["plan"]

    await session.end()
    assert poster.calls[-1] == ("swe", "D123", "1700000000.000100", "")
    assert driver.active_conversations == []


async def test_phase_transitions_swap_the_text():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)

    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    await session.set_phase("execute")
    await session.set_phase("execute")  # no-op — same phase, same words
    await session.set_phase("review")
    await session.end()

    plan, execute, review, cleared = poster.statuses
    assert plan in PHASE_PHRASES["plan"]
    assert execute in PHASE_PHRASES["execute"]
    assert review in PHASE_PHRASES["review"]
    assert cleared == ""


async def test_self_iterate_back_to_plan_shows_a_different_line():
    """A turn that loops Plan → Execute → Review → Plan must not re-show
    the line it opened with, or the indicator reads as stuck."""
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)

    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    first_plan = poster.statuses[0]
    await session.set_phase("execute")
    await session.set_phase("review")
    await session.set_phase("plan")
    await session.end()

    second_plan = poster.statuses[-2]
    assert second_plan in PHASE_PHRASES["plan"]
    assert second_plan != first_plan


async def test_two_threads_can_open_on_different_lines():
    """The turn_id is part of the seed, so the pool is actually used —
    a fixed line per phase would make every turn identical."""
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)

    openings = set()
    for i in range(len(PHASE_PHRASES["plan"]) * 4):
        meta = dict(SLACK_DM, channel=f"D{i}")
        session = await driver.begin(handle="swe", turn_id=f"t{i}", metadata=meta)
        assert session is not None
        openings.add(poster.statuses[-1])
        await session.end()

    assert len(openings) > 1


async def test_unknown_phase_falls_back_to_the_default_pool():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)
    session = await driver.begin(
        handle="swe", turn_id="t1", metadata=SLACK_DM, phase="onboarding"
    )
    assert session is not None
    assert poster.statuses[0] in PHASE_PHRASES["onboarding"]
    await session.set_phase("nonsense")
    assert poster.statuses[-1] in PHASE_PHRASES[DEFAULT_PHASE]
    await session.end()


async def test_custom_phrases_drive_the_indicator():
    poster = FakePoster()
    driver = WorkingStatusDriver(
        poster,
        phrases=StatusPhrases.with_overrides(
            {"plan": ["is nimbusing..."], "execute": ["is nimbusing it..."]}
        ),
    )
    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    await session.set_phase("execute")
    await session.end()
    assert poster.statuses == ["is nimbusing...", "is nimbusing it...", ""]


async def test_mode_addressed_skips_a_passive_channel_message():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster, WorkingStatusMode.ADDRESSED)
    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_PASSIVE)
    assert session is None
    assert poster.calls == []


async def test_mode_always_covers_a_passive_channel_message():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster, WorkingStatusMode.ALWAYS)
    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_PASSIVE)
    assert session is not None
    assert poster.statuses[0] in PHASE_PHRASES["plan"]
    await session.end()


async def test_mode_off_never_posts():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster, WorkingStatusMode.OFF)
    assert await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM) is None
    assert poster.calls == []


async def test_non_slack_trigger_opens_no_session():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)
    assert (
        await driver.begin(
            handle="swe",
            turn_id="t1",
            metadata={"transport": "jira", "issue_key": "POC-1"},
        )
        is None
    )
    assert poster.calls == []


async def test_two_turns_share_one_session_and_last_end_clears():
    """A suspend/resume pair — or a queued follow-up — must not clear the
    indicator while the other turn is still working."""
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)

    first = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_MENTION)
    second = await driver.begin(handle="swe", turn_id="t2", metadata=SLACK_MENTION)
    assert first is not None and second is not None
    assert len(driver.active_conversations) == 1

    await first.end()
    assert poster.statuses[-1] != ""  # still held by t2
    await second.end()
    assert poster.statuses[-1] == ""
    assert driver.active_conversations == []


async def test_keep_alive_holds_the_indicator_across_a_sandbox_suspend():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)

    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_MENTION)
    assert session is not None
    await session.set_phase("execute")
    await session.end(keep_alive=True)
    assert poster.statuses[-1] in PHASE_PHRASES["execute"]
    assert len(driver.active_conversations) == 1

    # The resumed turn re-enters under the SAME turn_id and closes it.
    resumed = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_MENTION)
    assert resumed is not None
    await resumed.end()
    assert poster.statuses[-1] == ""
    assert driver.active_conversations == []


async def test_heartbeat_refreshes_before_slack_expires_the_status():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster, refresh_interval=0.01)

    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    await asyncio.sleep(0.05)
    # The heartbeat re-asserts the SAME words — a status that changed
    # under a reader mid-phase would look like the agent restarted.
    opening = poster.statuses[0]
    assert opening in PHASE_PHRASES["plan"]
    refreshes = [s for s in poster.statuses if s == opening]
    assert len(refreshes) >= 2
    await session.end()
    assert poster.statuses[-1] == ""


async def test_heartbeat_drops_an_indicator_whose_agent_went_idle():
    """Safety net: a turn that dies without closing its session must not
    leave the agent looking like it is thinking forever."""
    poster = FakePoster()
    driver = WorkingStatusDriver(poster, refresh_interval=0.01)
    busy = {"value": True}

    session = await driver.begin(
        handle="swe",
        turn_id="t1",
        metadata=SLACK_DM,
        liveness=lambda: busy["value"],
    )
    assert session is not None
    busy["value"] = False
    await asyncio.sleep(0.05)

    assert poster.statuses[-1] == ""
    assert driver.active_conversations == []


async def test_session_expires_after_the_max_lifetime():
    poster = FakePoster()
    driver = WorkingStatusDriver(
        poster, refresh_interval=0.01, max_session_seconds=0.02
    )
    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    await asyncio.sleep(0.1)
    assert poster.statuses[-1] == ""
    assert driver.active_conversations == []
    # end() on an already-expired session is a safe no-op.
    await session.end()


async def test_stop_clears_every_live_indicator():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)
    await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    await driver.begin(handle="pm", turn_id="t2", metadata=SLACK_MENTION)
    assert len(driver.active_conversations) == 2

    await driver.stop()
    assert driver.active_conversations == []
    assert poster.statuses[-2:] == ["", ""]


async def test_a_failing_slack_api_never_raises():
    poster = FakePoster()
    poster.raises = True
    driver = WorkingStatusDriver(poster, refresh_interval=0.01)

    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    await session.set_phase("execute")
    await asyncio.sleep(0.03)
    await session.end()
    assert driver.active_conversations == []


async def test_rejected_slack_call_is_tolerated():
    poster = FakePoster(ok=False)
    driver = WorkingStatusDriver(poster)
    session = await driver.begin(handle="swe", turn_id="t1", metadata=SLACK_DM)
    assert session is not None
    await session.end()
    assert len(poster.statuses) == 2
    assert poster.statuses[0] in PHASE_PHRASES["plan"]
    assert poster.statuses[1] == ""


async def test_missing_handle_opens_no_session():
    poster = FakePoster()
    driver = WorkingStatusDriver(poster)
    assert await driver.begin(handle="", turn_id="t1", metadata=SLACK_DM) is None
    assert poster.calls == []
