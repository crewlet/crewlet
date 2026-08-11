"""Event (de)serialization shared by EventQueue backends.

Both the wire format (JSON via Pydantic) and the type-aware
reconstruction live here so every backend round-trips events
identically.  Keeping the registry in one place means a new event type
is picked up by every backend without per-backend edits.
"""

from __future__ import annotations

import json

from crewlet.events.types import Event


def _build_event_type_registry() -> dict[str, type[Event]]:
    """Build a mapping from event ``type`` strings to their model classes.

    Lets :func:`deserialize_event` reconstruct the correct subclass so
    typed fields (e.g. ``ExternalNotification.body``) survive a queue
    round-trip instead of being silently dropped by the base ``Event``
    model.
    """
    from crewlet.events import types as _t

    registry: dict[str, type[Event]] = {}
    for attr in dir(_t):
        cls = getattr(_t, attr)
        if isinstance(cls, type) and issubclass(cls, Event) and cls is not Event:
            # Use the class-level default for the ``type`` field.
            type_key = cls.model_fields["type"].default
            if isinstance(type_key, str) and type_key:
                registry[type_key] = cls
    return registry


_EVENT_TYPE_REGISTRY: dict[str, type[Event]] | None = None


def _get_registry() -> dict[str, type[Event]]:
    global _EVENT_TYPE_REGISTRY  # noqa: PLW0603
    if _EVENT_TYPE_REGISTRY is None:
        _EVENT_TYPE_REGISTRY = _build_event_type_registry()
    return _EVENT_TYPE_REGISTRY


def serialize_event(event: Event) -> bytes:
    """Serialize *event* to JSON bytes for transport."""
    return event.model_dump_json().encode()


def deserialize_event(data: bytes) -> Event:
    """Reconstruct an :class:`Event` (correct subclass) from JSON bytes.

    Peeks at the ``type`` field to choose the right subclass, falling
    back to the base ``Event`` for unknown types.
    """
    raw = json.loads(data)
    event_type = raw.get("type", "")
    cls = _get_registry().get(event_type, Event)
    return cls.model_validate(raw)
