"""Tests for turn-engine runtime guards."""

from __future__ import annotations

import pytest

from crewlet.agent.guards import (
    DelegationDepthExceeded,
    StallDetector,
    artifact_hash,
    check_delegation_depth,
)

# -- Delegation depth ----------------------------------------------------


def test_depth_ok_below_limit():
    check_delegation_depth(depth=2, limit=3)  # no raise


def test_depth_raises_at_limit():
    with pytest.raises(DelegationDepthExceeded):
        check_delegation_depth(depth=3, limit=3)


def test_depth_zero_limit_disables_cap():
    check_delegation_depth(depth=100, limit=0)  # no raise


# -- Stall detector ------------------------------------------------------


def test_stall_does_not_fire_before_threshold():
    sd = StallDetector(threshold=2)
    sd.observe("unchanged")
    assert sd.should_abort() is False


def test_stall_fires_when_artifact_unchanged():
    sd = StallDetector(threshold=2)
    sd.observe("unchanged")
    sd.observe("unchanged")
    assert sd.should_abort() is True


def test_stall_ignores_changes_before_threshold_tail():
    sd = StallDetector(threshold=2)
    sd.observe("v1")
    sd.observe("v2")
    sd.observe("v2")
    assert sd.should_abort() is True


def test_stall_does_not_fire_when_artifact_changes():
    sd = StallDetector(threshold=2)
    sd.observe("v1")
    sd.observe("v2")
    assert sd.should_abort() is False


def test_stall_reset_clears_history():
    sd = StallDetector(threshold=2)
    sd.observe("x")
    sd.observe("x")
    sd.reset()
    assert sd.should_abort() is False


def test_artifact_hash_stable_and_short():
    assert artifact_hash("x") == artifact_hash("x")
    assert artifact_hash("x") != artifact_hash("y")
    assert len(artifact_hash("x")) == 16
