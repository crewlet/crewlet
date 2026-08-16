"""Prompt rendering + envelope parsing for the ``cli-agent`` backend.

Both directions are pure, so every shape a model actually produces is
pinned here rather than discovered in an agent's first turn.
"""

from __future__ import annotations

from crewlet.providers.llm.cli_protocol import (
    estimate_tokens,
    json_path,
    parse_envelope,
    render_prompt,
)
from crewlet.providers.llm.protocol import Message, ToolCall, ToolDef

TOOL = ToolDef(
    name="lookup_colleague",
    description="Resolve an agent identifier.",
    parameters={"type": "object", "properties": {"who": {"type": "string"}}},
)


class TestRenderPrompt:
    def test_system_messages_lead(self):
        prompt = render_prompt(
            [
                Message(role="system", content="You are the CTO."),
                Message(role="user", content="Ship it."),
            ]
        )
        assert prompt.startswith("You are the CTO.")
        assert "Ship it." in prompt

    def test_no_tools_means_no_contract(self):
        prompt = render_prompt([Message(role="user", content="Summarise this.")])
        assert "Response format" not in prompt
        assert "tool_calls" not in prompt

    def test_tools_render_with_schema_and_contract(self):
        prompt = render_prompt([Message(role="user", content="go")], [TOOL])
        assert "lookup_colleague" in prompt
        assert "Response format" in prompt
        assert '"who"' in prompt

    def test_required_choice_is_stated(self):
        prompt = render_prompt(
            [Message(role="user", content="go")], [TOOL], tool_choice="required"
        )
        assert "MUST include at least one entry" in prompt

    def test_prior_tool_calls_and_results_are_visible(self):
        prompt = render_prompt(
            [
                Message(
                    role="assistant",
                    content="",
                    tool_calls=[
                        ToolCall(
                            id="1",
                            name="lookup_colleague",
                            arguments={"who": "pm"},
                        )
                    ],
                ),
                Message(
                    role="tool",
                    name="lookup_colleague",
                    tool_call_id="1",
                    content="PM is Dana.",
                ),
            ],
            [TOOL],
        )
        assert "called lookup_colleague" in prompt
        assert "PM is Dana." in prompt


class TestParseEnvelope:
    def test_fenced_envelope(self):
        content, calls = parse_envelope(
            'Sure!\n```json\n{"message": "on it", '
            '"tool_calls": [{"name": "t", "arguments": {"a": 1}}]}\n```'
        )
        assert content == "on it"
        assert [c.name for c in calls] == ["t"]
        assert calls[0].arguments == {"a": 1}

    def test_bare_object_without_a_fence(self):
        content, calls = parse_envelope('{"message": "hi", "tool_calls": []}')
        assert content == "hi"
        assert calls == []

    def test_last_fence_wins(self):
        content, _ = parse_envelope(
            '```json\n{"message": "draft"}\n```\n'
            'On reflection:\n```json\n{"message": "final"}\n```'
        )
        assert content == "final"

    def test_arguments_as_a_json_string(self):
        _content, calls = parse_envelope(
            '{"tool_calls": [{"name": "t", "arguments": "{\\"a\\": 2}"}]}'
        )
        assert calls[0].arguments == {"a": 2}

    def test_single_call_written_as_an_object(self):
        _content, calls = parse_envelope(
            '{"tool_calls": {"name": "t", "arguments": {}}}'
        )
        assert [c.name for c in calls] == ["t"]

    def test_alternate_argument_keys(self):
        _content, calls = parse_envelope(
            '{"tool_calls": [{"name": "t", "parameters": {"a": 3}}]}'
        )
        assert calls[0].arguments == {"a": 3}

    def test_unparseable_output_degrades_to_content(self):
        content, calls = parse_envelope("I could not comply.")
        assert content == "I could not comply."
        assert calls == []

    def test_empty_output(self):
        assert parse_envelope("") == ("", [])

    def test_incidental_json_is_not_mistaken_for_an_envelope(self):
        """A reply that quotes an unrelated object must not swallow the
        prose around it."""
        content, calls = parse_envelope(
            'The config is {"retries": 3} which looks fine.'
        )
        assert content.startswith("The config is")
        assert calls == []

    def test_content_synonyms_are_accepted(self):
        for key in ("message", "content", "text", "response"):
            content, _ = parse_envelope(f'{{"{key}": "ok", "tool_calls": []}}')
            assert content == "ok"

    def test_calls_get_distinct_ids(self):
        _content, calls = parse_envelope(
            '{"tool_calls": [{"name": "a"}, {"name": "b"}]}', id_prefix="p"
        )
        assert [c.id for c in calls] == ["p_0", "p_1"]

    def test_brace_inside_a_string_does_not_break_scanning(self):
        content, _ = parse_envelope('{"message": "use { and } freely"}')
        assert content == "use { and } freely"

    def test_non_string_message_is_serialised(self):
        content, _ = parse_envelope('{"message": {"a": 1}, "tool_calls": []}')
        assert content == '{"a": 1}'


class TestHelpers:
    def test_json_path_walks_and_misses_safely(self):
        payload = {"usage": {"input_tokens": 12}}
        assert json_path(payload, ["usage", "input_tokens"]) == 12
        assert json_path(payload, ["usage", "nope"]) is None
        assert json_path(payload, ["missing", "deep"]) is None
        assert json_path("scalar", ["a"]) is None

    def test_token_estimate_is_never_zero(self):
        assert estimate_tokens("") == 1
        assert estimate_tokens("a" * 400) == 100
