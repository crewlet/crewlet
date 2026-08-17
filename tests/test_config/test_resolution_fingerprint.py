"""The fingerprint that makes secret rotation actually rebuild anything."""

from __future__ import annotations

import pytest

from crewlet.config_resolution import referenced_names, resolution_fingerprint


def test_names_are_collected_from_anywhere_in_the_payload() -> None:
    payload = {
        "a": "${ONE}",
        "b": ["${TWO}", {"c": "prefix-${THREE}-suffix"}],
        "d": 5,
        "e": None,
    }
    assert referenced_names(payload) == ["ONE", "THREE", "TWO"]


def test_a_payload_with_no_references_is_stable() -> None:
    assert resolution_fingerprint({"a": "literal"}) == resolution_fingerprint(
        {"a": "literal"}
    )


def test_fingerprint_moves_when_a_referenced_value_rotates(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The whole point. The payload is byte-identical across a rotation —
    that is what re-activating an unchanged revision means — so the
    payload alone can never detect one."""
    payload = {"token": "${ROTATING_SECRET}"}

    monkeypatch.setenv("ROTATING_SECRET", "old-value")
    before = resolution_fingerprint(payload)

    monkeypatch.setenv("ROTATING_SECRET", "new-value")
    after = resolution_fingerprint(payload)

    assert before != after


def test_fingerprint_is_stable_when_nothing_rotated(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("STEADY", "same")
    payload = {"token": "${STEADY}"}
    assert resolution_fingerprint(payload) == resolution_fingerprint(payload)


def test_unset_and_empty_fingerprint_alike(monkeypatch: pytest.MonkeyPatch) -> None:
    """Deliberate: the config layer resolves an unset ${VAR} to the empty
    string and every builder consumes it that way, so reporting a
    difference here would disagree with what the builders actually see."""
    payload = {"token": "${MAYBE_SET}"}
    monkeypatch.delenv("MAYBE_SET", raising=False)
    unset = resolution_fingerprint(payload)
    monkeypatch.setenv("MAYBE_SET", "")
    assert unset == resolution_fingerprint(payload)


def test_values_cannot_collide_across_name_boundaries(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Length-prefixed encoding: ("ab","c") must not hash like ("a","bc")."""
    monkeypatch.setenv("AA", "ab")
    monkeypatch.setenv("BB", "c")
    one = resolution_fingerprint({"x": "${AA}", "y": "${BB}"})
    monkeypatch.setenv("AA", "a")
    monkeypatch.setenv("BB", "bc")
    two = resolution_fingerprint({"x": "${AA}", "y": "${BB}"})
    assert one != two


def test_the_digest_does_not_contain_the_secret(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """It is keyed per process precisely so a fingerprint that reaches a
    log or a row is not offline-brute-forceable."""
    monkeypatch.setenv("SEKRIT", "hunter2")
    assert "hunter2" not in resolution_fingerprint({"t": "${SEKRIT}"})
