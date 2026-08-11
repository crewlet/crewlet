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
        return self._cfg

    def set(self, cfg: TurnEngineConfig) -> None:
        """Swap the held config in one step.

        Concurrency: only the engine's ``apply_config`` calls this,
        and it holds ``Engine._apply_lock`` while doing so.  In-flight
        turns calling ``get()`` see either the old or new model
        atomically (Python attribute set is GIL-protected).
        """
        self._cfg = cfg
