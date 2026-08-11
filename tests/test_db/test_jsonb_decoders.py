"""Direct unit tests for the shared JSONB decoders.

Five caller modules across three subsystems (learning, knowledge,
timescaledb) route through ``crewlet.db._jsonb``; the decoders live
on the row-decoding hot path of every Postgres-backed read in those
subsystems.  Caller-side tests only exercise happy paths through the
stores, so the edge cases (malformed JSON, wrong inner types, mixed
list contents) need direct coverage here.
"""

from __future__ import annotations

import json
from uuid import uuid4

from crewlet.db._jsonb import (
    decode_jsonb_dict,
    decode_jsonb_str_list,
    decode_jsonb_uuid_list,
)

# ----------------------------------------------------------------------
# decode_jsonb_dict
# ----------------------------------------------------------------------


def test_dict_returns_already_decoded_dict_unchanged() -> None:
    src = {"a": 1, "b": [2, 3]}
    assert decode_jsonb_dict(src) is src  # asyncpg fast path: same object


def test_dict_parses_json_string() -> None:
    assert decode_jsonb_dict('{"k": "v"}') == {"k": "v"}


def test_dict_returns_empty_for_none() -> None:
    assert decode_jsonb_dict(None) == {}


def test_dict_returns_empty_for_unparseable_string() -> None:
    assert decode_jsonb_dict("{not json") == {}


def test_dict_returns_empty_when_string_parses_to_non_dict() -> None:
    assert decode_jsonb_dict("[1, 2, 3]") == {}


def test_dict_returns_empty_for_unrelated_types() -> None:
    assert decode_jsonb_dict(42) == {}
    assert decode_jsonb_dict([1, 2]) == {}


# ----------------------------------------------------------------------
# decode_jsonb_str_list
# ----------------------------------------------------------------------


def test_str_list_returns_already_decoded_list_coerced() -> None:
    assert decode_jsonb_str_list(["a", "b"]) == ["a", "b"]


def test_str_list_coerces_non_string_items() -> None:
    assert decode_jsonb_str_list([1, 2, None]) == ["1", "2", "None"]


def test_str_list_parses_json_string() -> None:
    assert decode_jsonb_str_list('["x", "y"]') == ["x", "y"]


def test_str_list_returns_empty_for_none() -> None:
    assert decode_jsonb_str_list(None) == []


def test_str_list_returns_empty_for_unparseable_string() -> None:
    assert decode_jsonb_str_list("[broken") == []


def test_str_list_returns_empty_when_string_parses_to_non_list() -> None:
    assert decode_jsonb_str_list('{"a": 1}') == []


def test_str_list_returns_empty_for_unrelated_types() -> None:
    assert decode_jsonb_str_list(42) == []
    assert decode_jsonb_str_list({"a": 1}) == []


# ----------------------------------------------------------------------
# decode_jsonb_uuid_list
# ----------------------------------------------------------------------


def test_uuid_list_returns_uuids_from_already_decoded_list() -> None:
    a, b = uuid4(), uuid4()
    assert decode_jsonb_uuid_list([str(a), str(b)]) == [a, b]


def test_uuid_list_accepts_uuid_objects_in_list() -> None:
    a = uuid4()
    assert decode_jsonb_uuid_list([a]) == [a]


def test_uuid_list_drops_non_uuid_items_silently() -> None:
    valid = uuid4()
    out = decode_jsonb_uuid_list([str(valid), "not-a-uuid", str(uuid4())])
    assert valid in out
    assert len(out) == 2  # the garbage entry was silently dropped


def test_uuid_list_parses_json_string() -> None:
    a = uuid4()
    encoded = json.dumps([str(a)])
    assert decode_jsonb_uuid_list(encoded) == [a]


def test_uuid_list_returns_empty_for_none() -> None:
    assert decode_jsonb_uuid_list(None) == []


def test_uuid_list_returns_empty_for_unparseable_string() -> None:
    assert decode_jsonb_uuid_list("[malformed") == []


def test_uuid_list_returns_empty_when_string_parses_to_non_list() -> None:
    assert decode_jsonb_uuid_list('{"a": 1}') == []


def test_uuid_list_returns_empty_for_unrelated_types() -> None:
    assert decode_jsonb_uuid_list(42) == []
    assert decode_jsonb_uuid_list({"a": 1}) == []
