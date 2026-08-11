"""YAML serialiser / deserialiser for :class:`CompanyConfig`.

Used by ``crewlet config import / export`` and the API's
``GET /config?format=yaml`` / ``PUT /config`` (Content-Type:
application/yaml) paths.

Round-trip property: ``company_config_from_yaml(company_config_to_yaml(c))``
returns a :class:`CompanyConfig` equal to ``c`` (modulo default
fill-ins).  Comments and YAML formatting are NOT preserved — this is
a data-level round-trip, not a textual one.
"""

from __future__ import annotations

from typing import Any

import yaml

from crewlet.config import CompanyConfig


def company_config_to_yaml(cfg: CompanyConfig) -> str:
    """Serialise a :class:`CompanyConfig` to a YAML string.

    Excludes ``None`` values and Python ``UNSET`` markers so the
    output is the minimal YAML that round-trips to an equivalent
    config.  ``${VAR}`` references in the input are preserved
    verbatim — env-var resolution happens at provider/transport
    construction time, never at serialisation.

    Dumps in ``mode="json"`` (not ``"python"``): ``yaml.safe_dump``
    resolves representers by exact type, so a ``StrEnum`` member
    (``integrations.slack.typing_status``, ``role.kind``) reaches it as
    an un-representable object and raises ``RepresenterError`` — which
    made ``crewlet config export`` and ``GET /config?format=yaml``
    fail outright on any config that set one.  JSON mode emits the
    plain scalar YAML can represent, and YAML is a JSON superset, so
    nothing else changes.
    """
    payload = company_config_to_dict(cfg)
    return yaml.safe_dump(payload, sort_keys=False, default_flow_style=False)


def company_config_to_dict(cfg: CompanyConfig) -> dict[str, Any]:
    """Serialise a :class:`CompanyConfig` to a JSON-safe dict.

    The DB store and the JSON API responses both want a dict; YAML
    is a thin wrapper on top.
    """
    return cfg.model_dump(mode="json", exclude_none=True)


def company_config_from_yaml(text: str) -> CompanyConfig:
    """Parse a YAML string into a validated :class:`CompanyConfig`."""
    raw = yaml.safe_load(text)
    if raw is None:
        raise ValueError("YAML body is empty")
    if not isinstance(raw, dict):
        raise ValueError("YAML body must be a mapping")
    return CompanyConfig.model_validate(raw)
