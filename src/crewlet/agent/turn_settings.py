"""Live-reference cell for ``TurnEngineConfig``.

:class:`TurnEngine` reads settings indirectly via :class:`TurnEngineSettings`
so the engine's hot-reload path can swap the config without recreating
the TurnEngine instance.  Each scalar setting on TurnEngine is a
``@property`` that delegates to ``self._settings.get().<field>`` —
in-flight turns continue to read through the cell, picking up updated
values on the next ``self._settings.get()`` call.

Holding a Pydantic model (rather than a frozen tuple) keeps the
contract identical for callers reading individual fields, and Pydantic
itself rejects invalid mutations because the model is immutable
post-construction; the cell hands out the new model in one shot via
``set(new_cfg)``.
"""

from __future__ import annotations

from crewlet.config import TurnEngineConfig


class TurnEngineSettings:
    """Mutable holder of a :class:`TurnEngineConfig` snapshot."""

    __slots__ = ("_cfg",)

    def __init__(self, cfg: TurnEngineConfig | None = None) -> None:
        self._cfg = cfg or TurnEngineConfig()

    def get(self) -> TurnEngineConfig:
        """The config in force — a turn's pinned snapshot, or the cell.

        The ~18 ``TurnEngine`` accessors call this on **every access**, so
        an un-pinned read lets one turn run Plan under one round cap and
        Execute under another, or size a sub-agent's budget from a
        fraction the parent never saw.  See :mod:`crewlet.agent.turn_pin`.
        """
        from crewlet.agent.turn_pin import current_pin

        pin = current_pin()
        if pin is not None and pin.settings is not None:
            return pin.settings
        return self._cfg

    def live(self) -> TurnEngineConfig:
        """The held config, ignoring any pin — what a new turn would take."""
        return self._cfg

    def set(self, cfg: TurnEngineConfig) -> None:
        """Swap the held config in one step.

        Concurrency: only the engine's ``apply_config`` calls this,
        and it holds ``Engine._apply_lock`` while doing so.  In-flight
        turns calling ``get()`` see either the old or new model
        atomically (Python attribute set is GIL-protected).
        """
        self._cfg = cfg
