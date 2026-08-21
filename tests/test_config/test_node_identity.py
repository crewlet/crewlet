"""Node identity: resolution precedence, validation, and log binding."""

from __future__ import annotations

import asyncio
import logging

import pytest

from crewlet.config import (
    DEFAULT_NODE_ID,
    BootstrapConfig,
    NodeConfig,
    resolve_node_id,
    resolve_node_incarnation,
)


def test_defaults_to_the_single_process_id(monkeypatch: pytest.MonkeyPatch) -> None:
    """Nothing has to be configured to run one engine."""
    monkeypatch.delenv("CREWLET_NODE_ID", raising=False)
    assert resolve_node_id(BootstrapConfig()) == DEFAULT_NODE_ID
    assert resolve_node_id(None) == DEFAULT_NODE_ID


def test_config_wins_over_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("CREWLET_NODE_ID", "from-env")
    bootstrap = BootstrapConfig(node=NodeConfig(id="from-config"))
    assert resolve_node_id(bootstrap) == "from-config"


def test_environment_fills_in_when_config_is_empty(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """How an orchestrator injects a pod name without templating the file."""
    monkeypatch.setenv("CREWLET_NODE_ID", "crewlet-2")
    assert resolve_node_id(BootstrapConfig()) == "crewlet-2"


def test_config_value_resolves_env_references(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MY_POD", "crewlet-7")
    bootstrap = BootstrapConfig(node=NodeConfig(id="${MY_POD}"))
    assert resolve_node_id(bootstrap) == "crewlet-7"


@pytest.mark.parametrize(
    "bad",
    ["-leading-dash", "has space", "has/slash", "a" * 65, "@node"],
)
def test_invalid_ids_are_rejected_in_config(bad: str) -> None:
    """The id lands in log fields and, later, broker subscription names."""
    with pytest.raises(ValueError):
        NodeConfig(id=bad)


def test_invalid_environment_id_is_rejected(monkeypatch: pytest.MonkeyPatch) -> None:
    """An orchestrator injecting a pod name with a '/' must fail loudly at
    boot, not surface later as a malformed subscription name."""
    monkeypatch.setenv("CREWLET_NODE_ID", "bad/name")
    with pytest.raises(ValueError):
        resolve_node_id(BootstrapConfig())


@pytest.mark.parametrize("ok", ["node-0", "crewlet-web-1", "host.example", "a_b", "n"])
def test_valid_ids_are_accepted(ok: str) -> None:
    assert NodeConfig(id=ok).id == ok


def test_incarnation_is_stable_within_a_process_and_carries_the_node_id(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The lease-holder identity. Stable per call so a caller cannot fence
    itself out by re-resolving, and prefixed with the node id so a lease
    row is still legible to an operator."""
    monkeypatch.setattr("crewlet.config._INCARNATION", "", raising=False)
    monkeypatch.setenv("CREWLET_NODE_ID", "crewlet-3")

    first = resolve_node_incarnation(BootstrapConfig())
    second = resolve_node_incarnation(BootstrapConfig())

    assert first == second
    assert first.startswith("crewlet-3:")
    assert first != "crewlet-3"


def test_incarnation_differs_from_the_stable_node_id(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The whole point: two engines defaulting to `node-0` must not share a
    lease-holder identity, or both would hold every seat at the same epoch."""
    monkeypatch.delenv("CREWLET_NODE_ID", raising=False)

    monkeypatch.setattr("crewlet.config._INCARNATION", "", raising=False)
    one = resolve_node_incarnation(BootstrapConfig())
    monkeypatch.setattr("crewlet.config._INCARNATION", "", raising=False)
    two = resolve_node_incarnation(BootstrapConfig())

    assert one != two
    assert one.startswith(f"{DEFAULT_NODE_ID}:")
    assert two.startswith(f"{DEFAULT_NODE_ID}:")


def test_bootstrap_without_a_node_block_still_validates() -> None:
    """Every existing config.yaml predates this field."""
    assert BootstrapConfig().node.id == ""


def test_binding_reaches_log_records_inside_the_event_loop(
    monkeypatch: pytest.MonkeyPatch, capfd: pytest.CaptureFixture[str]
) -> None:
    """`_bind_node_identity` runs synchronously before `asyncio.run`, so the
    binding has to survive into tasks created inside the loop — otherwise
    the field silently never appears on the lines that matter."""
    import structlog

    from crewlet import _logging
    from crewlet._logging import configure_logging, get_logger

    monkeypatch.setenv("CREWLET_NODE_ID", "crewlet-9")

    # `configure_logging` latches after its first call — correct for an
    # application, poison for a test that reads file-descriptor stderr.
    # `logging.StreamHandler(sys.stderr)` binds the stderr OBJECT, so
    # whichever earlier test first triggered the latch left the root
    # handler pointed at ITS pytest capture buffer, and this assertion
    # then read an empty `err` and failed — depending entirely on what
    # ran before it. Force a handler bound to the stderr `capfd` is
    # capturing now, and put the global state back afterwards so this
    # test does not do the same thing to the next one.
    root = logging.getLogger()
    saved_handlers, saved_level = list(root.handlers), root.level
    saved_latch = _logging._configured
    _logging._configured = False
    configure_logging(level=logging.INFO)
    structlog.contextvars.clear_contextvars()

    from crewlet.cli import _bind_node_identity

    try:
        assert _bind_node_identity(BootstrapConfig()) == "crewlet-9"

        async def _main() -> None:
            async def _inner() -> None:
                get_logger("test.node").info("inside_task")

            await asyncio.create_task(_inner())

        asyncio.run(_main())
    finally:
        structlog.contextvars.clear_contextvars()
        root.handlers[:] = saved_handlers
        root.setLevel(saved_level)
        _logging._configured = saved_latch

    # structlog renders the bound field into the emitted line itself, so
    # assert on the rendered output rather than on LogRecord attributes.
    err = capfd.readouterr().err
    assert "inside_task" in err
    assert "crewlet-9" in err, f"node= did not reach a line emitted in a task: {err}"
