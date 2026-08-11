"""Shared JSONB decoders for asyncpg row dicts.

asyncpg returns ``jsonb`` columns sometimes as a Python object and
sometimes as a raw JSON string, depending on whether a codec is
registered on the connection.  Every store hits the same decode
shape — string-then-parse-then-type-check — so the helpers live
once here and are reused across the learning, knowledge, and
timescaledb stores.

Each helper checks the already-decoded common case first so the
asyncpg-typical path stays a single ``isinstance`` away from
returning.
"""

from __future__ import annotations

import json
from typing import Any
from uuid import UUID


def decode_jsonb_dict(value: Any) -> dict[str, Any]:
    """Decode a JSONB value as a ``dict``.  Unparseable → ``{}``."""
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except Exception:
            return {}
        return parsed if isinstance(parsed, dict) else {}
    return {}


def decode_jsonb_str_list(value: Any) -> list[str]:
    """Decode a JSONB value as ``list[str]``.  Unparseable → ``[]``."""
    if isinstance(value, list):
        return [str(s) for s in value]
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except Exception:
            return []
        if isinstance(parsed, list):
            return [str(s) for s in parsed]
    return []


def decode_jsonb_uuid_list(value: Any) -> list[UUID]:
    """Decode a JSONB value as ``list[UUID]``.  Non-UUID items dropped."""
    if isinstance(value, list):
        items = value
    elif isinstance(value, str):
        try:
            parsed = json.loads(value)
        except Exception:
            return []
        if not isinstance(parsed, list):
            return []
        items = parsed
    else:
        return []
    out: list[UUID] = []
    for item in items:
        try:
            out.append(UUID(str(item)))
        except Exception:
            continue
    return out
