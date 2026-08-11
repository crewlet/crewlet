"""The two encodings of a validation rule must agree.

Most of Crewlet's config rules reach the JSON Schema for free, because
they are field constraints Pydantic already renders (types, enums,
``pattern``, ``additionalProperties: false``). A handful are genuinely
cross-field — the knowledge-backend exclusivity, a human seat needing a
contact — and Pydantic cannot derive those from a ``model_validator``.
Those are written out a second time as ``json_schema_extra``, next to
the validator they mirror.

Two encodings of one rule can drift, and a schema that disagrees with
the loader is worse than one that stays silent: an editor or an AI
authoring a config **without crewlet installed**
(``docs/getting-started/ai-authoring.md``) would trust it. This module
is the guarantee that they don't — every case below is run through both
paths and must get the same verdict.

Rules that JSON Schema genuinely *cannot* express — reference integrity
(``lead`` naming a real role, ``manages`` naming a real role or unit)
and anything needing external data (IANA timezones, cron semantics) —
are listed at the bottom as documented, deliberate gaps.
"""

from __future__ import annotations

import pytest
from jsonschema import Draft202012Validator
from pydantic import ValidationError

from crewlet.cli import build_config_schema
from crewlet.config import CompanyConfig, config_to_organization

_VALIDATOR = Draft202012Validator(build_config_schema("company"))


def _schema_rejects(doc: dict) -> bool:
    return bool(list(_VALIDATOR.iter_errors(doc)))


def _loader_rejects(doc: dict) -> bool:
    """Run the same two steps ``crewlet validate`` does.

    Not just ``model_validate``: several rules (the human-seat contact
    requirement, schedule cron/timezone, lead resolution) live on the
    runtime org models and only fire when the ``Organization`` is built.
    The schema's job is to agree with what the *command* accepts, so
    that is what this compares against.
    """
    try:
        config_to_organization(CompanyConfig.model_validate(doc))
    except (ValidationError, ValueError):
        return True
    return False


_PLANE = {
    "enabled": True,
    "url": "https://plane.example",
    "workspace": "acme",
    "webhook_secret": "s",
}

_CONFLUENCE = {"url": "https://x.atlassian.net/wiki", "token": "t"}

#: ``(id, document)`` — each must be rejected by BOTH encodings.
INVALID: list[tuple[str, dict]] = [
    # Field-level: rendered by Pydantic, no hand-written schema.
    ("role field typo", {"name": "A", "roles": [{"name": "X", "backstroy": "y"}]}),
    ("unit field typo", {"name": "A", "units": [{"name": "E", "leed": "X"}]}),
    ("mcp server typo", {"name": "A", "mcp_servers": [{"name": "s", "commnad": "c"}]}),
    ("bad role kind", {"name": "A", "roles": [{"name": "X", "kind": "robot"}]}),
    (
        "bad llm provider type",
        {"name": "A", "providers": {"llm": {"default": {"type": "openaii"}}}},
    ),
    (
        "bad embeddings type",
        {"name": "A", "providers": {"embeddings": {"type": "cohere"}}},
    ),
    ("bad handle format", {"name": "A", "roles": [{"name": "X", "handle": "Not Ok!"}]}),
    (
        "cron with wrong field count",
        {
            "name": "A",
            "roles": [
                {
                    "name": "X",
                    "schedules": [{"name": "s", "cron": "* * *", "task": "t"}],
                }
            ],
        },
    ),
    ("bad skill_variables key", {"name": "A", "skill_variables": {"base-url": "u"}}),
    # Cross-field: the hand-written json_schema_extra rules.
    (
        "human seat without contact",
        {"name": "A", "roles": [{"name": "Jane", "kind": "human"}]},
    ),
    (
        "confluence and plane both active",
        {
            "name": "A",
            "integrations": {
                "confluence": _CONFLUENCE,
                "plane": _PLANE,
            },
        },
    ),
    (
        "plane_projects without plane enabled",
        {"name": "A", "knowledge": {"plane_projects": ["P"]}},
    ),
    (
        "confluence_spaces while plane enabled",
        {
            "name": "A",
            "integrations": {"plane": _PLANE},
            "knowledge": {"confluence_spaces": ["H"]},
        },
    ),
]

#: Configs that must be accepted by BOTH — the guard against a rule
#: that rejects too much.
VALID: list[tuple[str, dict]] = [
    ("minimal", {"name": "A"}),
    ("agent seat", {"name": "A", "roles": [{"name": "CEO", "goal": "lead"}]}),
    (
        "human seat with contact",
        {
            "name": "A",
            "roles": [
                {"name": "Jane", "kind": "human", "contact": {"slack_user_id": "U1"}}
            ],
        },
    ),
    ("auto-derived (empty) handle", {"name": "A", "roles": [{"name": "X"}]}),
    ("explicit valid handle", {"name": "A", "roles": [{"name": "X", "handle": "x-1"}]}),
    (
        "valid five-field cron",
        {
            "name": "A",
            "roles": [
                {
                    "name": "X",
                    "schedules": [
                        {"name": "s", "cron": "*/5 9-17 * * 1-5", "task": "t"}
                    ],
                }
            ],
        },
    ),
    (
        "plane backend with its scope",
        {
            "name": "A",
            "integrations": {"plane": _PLANE},
            "knowledge": {"plane_projects": ["P"]},
        },
    ),
    (
        "confluence backend with its scope",
        {
            "name": "A",
            "integrations": {"confluence": _CONFLUENCE},
            "knowledge": {"confluence_spaces": ["H"]},
        },
    ),
    (
        "inert disabled plane alongside confluence",
        {
            "name": "A",
            "integrations": {
                "confluence": _CONFLUENCE,
                "plane": {**_PLANE, "enabled": False},
            },
        },
    ),
    ("valid skill_variables key", {"name": "A", "skill_variables": {"base_url": "u"}}),
    (
        "every supported llm provider type",
        {
            "name": "A",
            "providers": {
                "llm": {
                    "a": {"type": "openai"},
                    "b": {"type": "anthropic"},
                    "c": {"type": "openai-compatible", "base_url": "https://x/v1/"},
                }
            },
        },
    ),
]


@pytest.mark.parametrize("doc", [d for _, d in INVALID], ids=[i for i, _ in INVALID])
def test_invalid_config_rejected_by_both_encodings(doc: dict) -> None:
    assert _loader_rejects(doc), "the loader accepts this — rule missing in Python"
    assert _schema_rejects(doc), (
        "the loader rejects this but the JSON Schema does not — a "
        "schema-only consumer (editor / CI / AI without crewlet) would "
        "be told this config is fine"
    )


@pytest.mark.parametrize("doc", [d for _, d in VALID], ids=[i for i, _ in VALID])
def test_valid_config_accepted_by_both_encodings(doc: dict) -> None:
    assert not _loader_rejects(doc), "the loader rejects a config that should pass"
    assert not _schema_rejects(doc), (
        "the JSON Schema rejects a config the loader accepts — a rule "
        "is over-broad and would flag valid YAML in the editor"
    )


class TestDocumentedSchemaGaps:
    """Rules JSON Schema cannot express. Pinned so the limitation stays
    a known, tested fact rather than an assumption — the skill tells the
    assistant to check these by reading, and
    ``docs/getting-started/ai-authoring.md`` lists them.
    """

    @pytest.mark.parametrize(
        ("case", "doc"),
        [
            (
                "invalid timezone (needs the IANA database)",
                {
                    "name": "A",
                    "roles": [
                        {
                            "name": "X",
                            "schedules": [
                                {
                                    "name": "s",
                                    "cron": "* * * * *",
                                    "task": "t",
                                    "timezone": "Mars/Olympus",
                                }
                            ],
                        }
                    ],
                },
            ),
            (
                "cron that is the right shape but semantically invalid",
                {
                    "name": "A",
                    "roles": [
                        {
                            "name": "X",
                            "schedules": [
                                {"name": "s", "cron": "99 * * * *", "task": "t"}
                            ],
                        }
                    ],
                },
            ),
        ],
    )
    def test_caught_by_loader_only(self, case: str, doc: dict) -> None:
        assert _loader_rejects(doc), case
        assert not _schema_rejects(doc), (
            f"{case}: the schema now catches this — move it out of the "
            "documented-gaps list and into INVALID above"
        )
