"""Body parsing + structural-diff helpers used by ``/config/*`` routes.

Extracted from ``config_routes.py`` so the route handlers stay focused
on HTTP-level concerns (status codes, header parsing, dispatcher
calls) while the YAML/JSON content negotiation + diff generation
live in their own module that's easier to test in isolation.
"""

from __future__ import annotations

import json
from typing import Any

import yaml
from starlette.requests import Request


async def parse_request_body(request: Request) -> dict[str, Any]:
    """Parse a JSON or YAML request body based on Content-Type.

    Accepts ``Content-Type: application/yaml`` (or any header
    containing ``yaml``) for YAML; ``application/json`` /
    ``text/json`` / missing header default to JSON.  Anything else
    (``text/xml``, ``application/x-www-form-urlencoded``, raw bytes
    with a non-text content type) raises ``ValueError`` with a
    helpful hint instead of a cryptic parse error.  Empty bodies and
    non-mapping payloads also raise ``ValueError``.
    """
    body = await request.body()
    if not body:
        raise ValueError("empty request body")
    content_type = (request.headers.get("content-type") or "").lower()
    if "yaml" in content_type:
        loaded = yaml.safe_load(body)
    elif content_type == "" or "json" in content_type:
        loaded = json.loads(body)
    else:
        raise ValueError(
            f"unsupported content-type {content_type!r}; "
            f"use 'application/json' or 'application/yaml'"
        )
    if not isinstance(loaded, dict):
        raise ValueError("request body must be a mapping")
    return loaded


def structural_diff(
    old: dict[str, Any], new: dict[str, Any], path: str = ""
) -> list[dict[str, Any]]:
    """Return a list of ``{op, path, old?, new?}`` diff entries.

    A deliberately simple structural diff — not full RFC-6902
    JSON-patch.  Each entry's ``op`` is one of ``"add"``,
    ``"remove"``, or ``"replace"``; ``path`` is a JSON Pointer-style
    string (``"/units/0/name"``).
    """
    out: list[dict[str, Any]] = []
    old_keys = set(old) if isinstance(old, dict) else set()
    new_keys = set(new) if isinstance(new, dict) else set()
    for key in sorted(old_keys | new_keys):
        sub_path = f"{path}/{key}" if path else f"/{key}"
        in_old, in_new = key in old_keys, key in new_keys
        if in_old and not in_new:
            out.append({"op": "remove", "path": sub_path, "old": old[key]})
        elif in_new and not in_old:
            out.append({"op": "add", "path": sub_path, "new": new[key]})
        else:
            ov, nv = old[key], new[key]
            if isinstance(ov, dict) and isinstance(nv, dict):
                out.extend(structural_diff(ov, nv, sub_path))
            elif ov != nv:
                out.append({"op": "replace", "path": sub_path, "old": ov, "new": nv})
    return out
