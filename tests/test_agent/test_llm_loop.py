"""Tests for the shared tool-call loop."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from crewlet.agent.llm_loop import (
    LoopResult,
    _assistant_text_with_reasoning,
    execute_tool,
    run_tool_loop,
    sanitize_tool_output,
    validate_tool_result,
)
from crewlet.providers.llm.protocol import Completion, Message, ToolCall
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface

# -- Helpers --------------------------------------------------------------


async def _echo(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    return ToolResult(success=True, output=params.get("text", ""))


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


@dataclass
class _ProviderStub:
    """Returns canned completions in sequence on each ``complete`` call."""

    completions: list[Completion]
    calls: int = 0
    model: str = "stub"

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        idx = self.calls
        self.calls += 1
        if idx >= len(self.completions):
            return Completion(content="done", tool_calls=[])
        return self.completions[idx]


class _AgentStub:
    """Minimal AgentInstance stub (attributes accessed by the loop)."""

    def __init__(self, handle: str = "sarah") -> None:
        self.handle = handle
        self.role_name = "Engineer"
        self.id_str = "agent-1"
        self.input_tokens = 0
        self.output_tokens = 0


@pytest.fixture
def surface() -> ToolSurface:
    reg = ToolRegistry()
    reg.register(
        SimpleTool(
            name="echo",
            description="echo the text back",
            parameters={"type": "object"},
            fn=_echo,
        )
    )
    return ToolSurface.for_execute(
        reg,
        role_mcp_tools=[],
        tools_needed=["echo"],
        always_on=[],
    )


@pytest.fixture
def ctx() -> AgentContext:
    return AgentContext(agent_id="agent-1", agent_handle="sarah", role="Engineer")


# -- Helpers (sanitize / validate) ----------------------------------------


def test_sanitize_strips_control_chars():
    text = "hello\x01\x02 world\x7f"
    assert sanitize_tool_output(text) == "hello world"


def test_sanitize_preserves_newlines_tabs():
    text = "line1\nline2\tcolumn"
    assert sanitize_tool_output(text) == text


def test_sanitize_does_not_truncate_long_output():
    # Tool output is never length-truncated: the agent must see the full
    # result (e.g. the tail of a list_mcp_server_tools listing where the
    # tool it needs may sort past any cap).
    text = "x" * 100000
    out = sanitize_tool_output(text)
    assert out == text
    assert "truncated" not in out


def test_validate_rejects_binary_data():
    result = ToolResult(success=True, output="hello\x00there")
    validated = validate_tool_result(result)
    assert validated.success is False
    assert "binary" in (validated.error or "").lower()


def test_validate_redacts_secrets():
    result = ToolResult(success=True, output="GITHUB_TOKEN=ghp_" + "a" * 40)
    validated = validate_tool_result(result)
    assert validated.success is True
    assert "[REDACTED:github-token]" in validated.output
    assert "ghp_" + "a" * 40 not in validated.output


def test_validate_redacts_plane_tokens():
    """Plane API tokens (``plane_api_…``, CE ``APIToken.generate_token``)
    and webhook secrets (``plane_wh_…``) are redacted everywhere tool
    output / transcripts flow."""
    api_token = "plane_api_" + "a" * 30
    wh_secret = "plane_wh_" + "b" * 30
    result = ToolResult(
        success=True, output=f"PLANE_API_KEY={api_token} webhook {wh_secret}"
    )
    validated = validate_tool_result(result)
    assert validated.success is True
    assert "[REDACTED:plane-token]" in validated.output
    assert "[REDACTED:plane-webhook-secret]" in validated.output
    assert api_token not in validated.output
    assert wh_secret not in validated.output


def test_validate_leaves_bare_plane_prefix_untouched():
    """The literal prefixes without a 20+ char tail (docs, error
    messages naming the format) must not be redacted."""
    text = "tokens use the plane_api_ prefix; secrets use plane_wh_abc"
    result = ToolResult(success=True, output=text)
    validated = validate_tool_result(result)
    assert validated.output == text


def test_validate_runs_custom_validators():
    class Reject:
        name = "reject"

        def validate(self, output: str) -> str:
            raise ValueError("nope")

    result = ToolResult(success=True, output="ok")
    validated = validate_tool_result(result, validators=[Reject()])
    assert validated.success is False
    assert "Validation failed" in (validated.error or "")


def test_assistant_text_with_reasoning_wraps_in_think_tags():
    """Dashboard renders ``<think>...</think>`` blocks inline -- so
    the response field on ``AgentPhaseCompleted`` must embed each
    assistant message's ``reasoning_content`` wrapped in those
    tags, immediately before that round's visible content.
    """
    messages = [
        Message(role="system", content="sys"),
        Message(role="user", content="do thing"),
        Message(
            role="assistant",
            content="Round 1 answer.",
            reasoning_content="Let me think...",
        ),
        Message(
            role="assistant",
            content="Final answer.",
            reasoning_content="Now I'm sure.",
        ),
    ]
    text = _assistant_text_with_reasoning(messages)
    assert "<think>Let me think...</think>" in text
    assert "<think>Now I'm sure.</think>" in text
    assert "Round 1 answer." in text
    assert "Final answer." in text
    # Thinking precedes its round's content.
    assert text.index("<think>Let me think...</think>") < text.index("Round 1 answer.")


def test_assistant_text_with_reasoning_no_reasoning_falls_back_to_content():
    messages = [
        Message(role="user", content="hi"),
        Message(role="assistant", content="hey"),
    ]
    text = _assistant_text_with_reasoning(messages)
    assert text == "hey"
    assert "<think>" not in text


async def test_execute_tool_plan_phase_direct_call_rejected(ctx):
    """A direct catalogue-tool call from Plan that hasn't been
    activated yet is rejected -- the planner must first call
    ``activate_tool`` to promote it.  The rejection error points at
    the activation path and tells the LLM that activation is what
    makes the tool callable, so the planner doesn't loop on retrying
    the bare call.
    """
    registry = ToolRegistry()
    registry.register(
        SimpleTool(
            name="lookup_colleague",
            description="Look up an agent.",
            parameters={"type": "object"},
            fn=_echo,
        )
    )

    submit = SimpleTool(
        name="submit_plan",
        description="Submit plan.",
        parameters={"type": "object"},
        fn=_echo,
    )
    # activate_tool must be on the surface for the rich activation
    # error to fire (the error directs the LLM to call activate_tool;
    # without it on the surface that would be misleading).
    activate = SimpleTool(
        name="activate_tool",
        description="Activate.",
        parameters={"type": "object"},
        fn=_echo,
    )
    surface = ToolSurface.for_plan(
        registry, role_mcp_tools=[], meta_tools=[submit, activate]
    )

    assert surface.has("lookup_colleague") is False
    assert "lookup_colleague" in surface.catalogue_names()

    result = await execute_tool(
        "lookup_colleague",
        {"name": "U0TESTUSER4"},
        ctx,
        surface=surface,
    )
    assert result.success is False
    err = result.error or ""
    # The rejected name still appears so the planner can match the
    # error to its own call.
    assert "lookup_colleague" in err
    # The message points at activate_tool as the activation path and
    # explicitly tells the LLM the call promotes the tool into
    # tools=[...] -- without this hint the planner re-tries the bare
    # call after activating, expecting a different result.
    assert "activate_tool" in err
    assert "promotes" in err
    # And the "no need to re-activate" line discourages the planner
    # from re-calling activate_tool after the tool is already active.
    assert "re-activate" in err
    # execute_tool itself never mutates the surface -- activation only
    # happens inside the activate_tool closure.
    assert surface.has("lookup_colleague") is False


async def test_execute_tool_plan_phase_unknown_totally_unknown_error(ctx):
    """Plan-phase unknown name that isn't in the catalogue gets a
    hint pointing back at the catalogue (with the ``list_mcp_server_tools``
    discovery suggestion only when ``activate_tool`` IS on the
    surface), not a misleading ``activate_tool(name=X)`` suggestion
    for a name that doesn't exist."""
    registry = ToolRegistry()
    submit = SimpleTool(
        name="submit_plan",
        description="Submit plan.",
        parameters={"type": "object"},
        fn=_echo,
    )
    activate = SimpleTool(
        name="activate_tool",
        description="Activate.",
        parameters={"type": "object"},
        fn=_echo,
    )
    surface = ToolSurface.for_plan(
        registry, role_mcp_tools=[], meta_tools=[submit, activate]
    )

    result = await execute_tool("nonsense_tool", {}, ctx, surface=surface)
    assert result.success is False
    err = result.error or ""
    assert "PLAN-phase catalogue" in err
    # No suggestion to call ``activate_tool(name="nonsense_tool")`` —
    # that would be misleading for a name not in the catalogue.
    assert "activate_tool(name='nonsense_tool')" not in err


async def test_execute_tool_execute_phase_unknown_points_at_discovery(ctx):
    """Execute-phase unknown-tool error points the LLM at the
    discovery surface (``## Available tools`` block +
    ``list_mcp_server_tools``) so it can recover instead of giving
    up on the bare 'Unknown tool' string.
    """
    registry = ToolRegistry()
    activate = SimpleTool(
        name="activate_tool",
        description="Activate.",
        parameters={"type": "object"},
        fn=_echo,
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        meta_tools=[activate],
    )
    result = await execute_tool("nonsense_tool", {}, ctx, surface=surface)
    assert result.success is False
    err = result.error or ""
    assert "nonsense_tool" in err
    assert "EXECUTE-phase catalogue" in err


async def test_execute_tool_grace_path_falls_back_to_bare_unknown(ctx):
    """Execute grace / rescue surfaces are built without the
    discovery meta-tools (``expose_catalogue=False`` + no
    ``meta_tools``). Telling the LLM to call ``activate_tool`` /
    ``list_mcp_server_tools`` there would direct it at tools it
    doesn't have — fall back to the bare error.
    """
    registry = ToolRegistry()
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        expose_catalogue=False,
    )
    result = await execute_tool("nonsense_tool", {}, ctx, surface=surface)
    assert result.success is False
    err = result.error or ""
    assert err == "Unknown tool: nonsense_tool"
    assert "activate_tool" not in err
    assert "list_mcp_server_tools" not in err


async def test_execute_tool_execute_phase_catalogue_tool_suggests_activate(ctx):
    """When the executor calls a name that IS in the catalogue but not
    yet active, the error tells it to call ``activate_tool`` — the same
    recovery path Plan uses; Execute supports it too.
    """
    registry = ToolRegistry()
    registry.register(
        SimpleTool(
            name="search",
            description="Search.",
            parameters={"type": "object"},
            fn=_echo,
        )
    )
    activate = SimpleTool(
        name="activate_tool",
        description="Activate.",
        parameters={"type": "object"},
        fn=_echo,
    )
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools=[],
        tools_needed=[],
        always_on=[],
        meta_tools=[activate],
    )
    # ``search`` is in the catalogue (registry-merged) but not in tools_needed.
    assert "search" in surface.catalogue_names()
    assert surface.has("search") is False
    result = await execute_tool("search", {}, ctx, surface=surface)
    assert result.success is False
    err = result.error or ""
    assert "activate_tool" in err
    assert "search" in err


# -- Loop ------------------------------------------------------------------


async def test_loop_terminates_on_no_tool_calls(surface, ctx):
    provider = _ProviderStub(
        completions=[Completion(content="hello world", tool_calls=[])]
    )
    queue = _QueueStub()
    result: LoopResult = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="hi")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
    )
    assert result.text == "hello world"
    assert result.rounds_used == 1
    assert result.exhausted_rounds is False
    assert result.tool_executions == []


async def test_loop_required_retries_when_model_returns_no_tool_call(surface, ctx):
    # With tool_choice="required" a prose-only response (the model
    # "thought" but emitted no tool call) must be re-prompted and retried,
    # not accepted as a clean finish — this is what makes the plan rescue /
    # extension judge actually force their structured-output call.
    provider = _ProviderStub(
        completions=[
            Completion(content="<think>I should call echo</think>", tool_calls=[]),
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="echo", arguments={"text": "x"})],
            ),
        ]
    )
    queue = _QueueStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=3,
        event_queue=queue,
        terminate_after=["echo"],
        tool_choice="required",
    )
    # The loop retried and got the tool call on the second attempt.
    assert any(e["name"] == "echo" for e in result.tool_executions)
    assert result.rounds_used == 2
    # A corrective re-prompt was injected before the retry.
    assert any(
        m.role == "user" and "no tool call" in m.content for m in result.messages
    )


async def test_loop_auto_does_not_retry_on_no_tool_call(surface, ctx):
    # Without tool_choice="required" a prose-only response is a legitimate
    # "I'm done" — the loop must NOT force a retry (that would break normal
    # Plan/Execute turns that end with a text answer).
    provider = _ProviderStub(
        completions=[Completion(content="all done", tool_calls=[])]
    )
    queue = _QueueStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
    )
    assert result.rounds_used == 1
    assert provider.calls == 1


async def test_loop_required_gives_up_after_bounded_retries(surface, ctx):
    # A model that can NEVER emit a tool call must not spin forever; the
    # retry is bounded (by the cap and by max_rounds) and the loop returns
    # exhausted rather than hanging.
    provider = _ProviderStub(completions=[])  # always prose ("done")
    queue = _QueueStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=2,
        event_queue=queue,
        terminate_after=["echo"],
        tool_choice="required",
    )
    assert result.rounds_used <= 2
    assert not any(e["name"] == "echo" for e in result.tool_executions)


async def test_loop_executes_tool_and_returns_final_text(surface, ctx):
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(id="c1", name="echo", arguments={"text": "hi there"})
                ],
            ),
            Completion(content="final answer", tool_calls=[]),
        ]
    )
    queue = _QueueStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="please echo")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
    )
    assert result.text == "final answer"
    assert len(result.tool_executions) == 1
    assert result.tool_executions[0]["name"] == "echo"
    # Progress event published between rounds.
    assert any(topic.startswith("crewlet.events.") for topic, _ in queue.published)


async def test_loop_progress_events_carry_turn_coordinates(surface, ctx):
    """Per-round ``AgentTurnProgress`` events carry the turn coordinates
    (``turn_id`` / ``phase`` / ``iteration`` / ``role``) so live
    consumers (the dashboard agent page) can place in-flight rounds
    inside the right turn/phase grouping — the same coordinates the
    ``AgentPhaseStarted`` / ``AgentPhaseCompleted`` pair carries.
    """
    from crewlet.events.types import AgentTurnProgress

    provider = _ProviderStub(
        completions=[
            Completion(
                content="working",
                tool_calls=[ToolCall(id="c1", name="echo", arguments={"text": "hi"})],
            ),
            Completion(content="final", tool_calls=[]),
        ]
    )
    queue = _QueueStub()
    await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="please echo")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
        turn_id="turn-abc",
        iteration=2,
    )
    progress = [e for _, e in queue.published if isinstance(e, AgentTurnProgress)]
    assert progress, "expected a progress event after the tool round"
    ev = progress[0]
    assert ev.turn_id == "turn-abc"
    assert ev.iteration == 2
    # Phase comes from the surface (``for_execute`` here).
    assert ev.phase == "execute"
    assert ev.role == "Engineer"
    assert ev.tool_executions and ev.tool_executions[0]["name"] == "echo"


async def test_loop_progress_events_carry_trigger_source(surface, ctx):
    """The ``trigger`` descriptor (the event that caused the turn) rides
    on every per-round ``AgentTurnProgress`` so a live dashboard row
    keeps its source across the round-by-round overwrites."""
    from crewlet.events.types import AgentTurnProgress

    provider = _ProviderStub(
        completions=[
            Completion(
                content="working",
                tool_calls=[ToolCall(id="c1", name="echo", arguments={"text": "hi"})],
            ),
            Completion(content="final", tool_calls=[]),
        ]
    )
    queue = _QueueStub()
    trigger = {"id": "ev-1", "type": "task_assigned", "summary": "Do the thing"}
    await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="please echo")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
        turn_id="turn-abc",
        iteration=0,
        trigger=trigger,
    )
    progress = [e for _, e in queue.published if isinstance(e, AgentTurnProgress)]
    assert progress and progress[0].trigger == trigger


async def test_loop_progress_phase_falls_back_to_provider_key(ctx):
    """Loops run against a surface without a ``phase`` tag (ad-hoc
    callers) fall back to ``provider_key`` — mirroring the
    ``prompt.size`` event's convention."""
    from crewlet.events.types import AgentTurnProgress

    class _BareSurface:
        def to_tool_defs(self):
            return [
                {
                    "type": "function",
                    "function": {
                        "name": "echo",
                        "description": "echo",
                        "parameters": {"type": "object"},
                    },
                }
            ]

        def lookup(self, name):
            return None  # unknown-tool path; the round still completes

    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="echo", arguments={"text": "x"})],
            ),
            Completion(content="done", tool_calls=[]),
        ]
    )
    queue = _QueueStub()
    await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=_BareSurface(),
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
        provider_key="auxiliary",
    )
    progress = [e for _, e in queue.published if isinstance(e, AgentTurnProgress)]
    assert progress
    assert progress[0].phase == "auxiliary"
    assert progress[0].turn_id == ""


async def test_loop_exhausts_rounds_and_flags_exhausted(surface, ctx):
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id=f"c{i}", name="echo", arguments={"text": "x"})],
            )
            for i in range(3)
        ]
    )
    queue = _QueueStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="loop forever")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=3,
        event_queue=queue,
    )
    assert result.exhausted_rounds is True
    assert result.rounds_used == 3


async def test_loop_rejects_tool_not_in_surface(surface, ctx):
    """A tool call for a name not in the surface yields an unknown-tool error."""
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="not_there", arguments={})],
            ),
            Completion(content="giving up", tool_calls=[]),
        ]
    )
    queue = _QueueStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="try a missing tool")],
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=queue,
    )
    # The loop continued; the failed tool result was fed back as a "tool"
    # message. The error string quotes the name for clarity
    # (``Unknown tool: 'not_there'. ...``), so we substring-match on
    # both forms.
    assert any(
        "not_there" in exe["result"] and "Unknown tool" in exe["result"]
        for exe in result.tool_executions
    )


async def test_loop_appends_terminal_assistant_message_on_no_tool_calls(surface, ctx):
    """When the provider returns a completion with no tool_calls, the
    assistant Message must still be
    appended to ``messages`` so downstream fallback parsers
    (``parse_plan_from_messages`` / ``parse_review_from_messages``)
    and debuggers can see what the model actually said.
    """
    messages: list[Message] = [Message(role="user", content="hi")]
    provider = _ProviderStub(
        completions=[
            Completion(
                content='{"decision":"done","final_artifact":"ok"}',
                tool_calls=[],
                reasoning_content="thinking...",
            )
        ]
    )
    await run_tool_loop(
        provider=provider,
        messages=messages,
        surface=surface,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=_QueueStub(),
    )
    # messages == [user, assistant]
    assert len(messages) == 2
    assert messages[-1].role == "assistant"
    assert messages[-1].content == '{"decision":"done","final_artifact":"ok"}'
    assert messages[-1].tool_calls == []
    assert messages[-1].reasoning_content == "thinking..."


async def test_loop_accumulates_token_totals(surface, ctx):
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="c1", name="echo", arguments={"text": "x"})],
                input_tokens=10,
                output_tokens=5,
                tokens_used=15,
            ),
            Completion(
                content="done",
                tool_calls=[],
                input_tokens=20,
                output_tokens=7,
                tokens_used=27,
            ),
        ]
    )
    queue = _QueueStub()
    agent = _AgentStub()
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="hi")],
        surface=surface,
        context=ctx,
        agent=agent,
        max_rounds=5,
        event_queue=queue,
    )
    assert result.input_tokens == 30
    assert result.output_tokens == 12
    assert agent.input_tokens == 30
    assert agent.output_tokens == 12


# -- Suspend/resume primitive ---------------------------------------------


async def _launch(params: dict[str, Any], ctx: AgentContext) -> ToolResult:
    """A tool that kicks off detached work and suspends the loop."""
    return ToolResult(
        success=True, output="", suspend=True, suspend_payload={"sandbox_id": "sbx-1"}
    )


def _suspend_surface(tools_needed: list[str]) -> ToolSurface:
    reg = ToolRegistry()
    reg.register(
        SimpleTool(
            name="echo",
            description="echo the text back",
            parameters={"type": "object"},
            fn=_echo,
        )
    )
    reg.register(
        SimpleTool(
            name="run_sandbox",
            description="launch detached work",
            parameters={"type": "object"},
            fn=_launch,
        )
    )
    return ToolSurface.for_execute(
        reg, role_mcp_tools=[], tools_needed=tools_needed, always_on=[]
    )


async def test_loop_suspends_and_leaves_pending_call_unanswered(ctx):
    # An ``allow_suspend`` loop (Execute): a tool returning suspend=True ends
    # the loop, leaving its call unanswered for the engine to resume later.
    surf = _suspend_surface(["run_sandbox"])
    provider = _ProviderStub(
        completions=[
            Completion(
                content="working",
                tool_calls=[ToolCall(id="s1", name="run_sandbox", arguments={})],
            ),
            Completion(content="must not reach", tool_calls=[]),
        ]
    )
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surf,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=_QueueStub(),
        allow_suspend=True,
    )
    assert result.suspended is True
    assert result.pending_tool_call_id == "s1"
    assert result.pending_tool_name == "run_sandbox"
    assert result.suspend_payload == {"sandbox_id": "sbx-1"}
    assert result.exhausted_rounds is False
    # The loop stopped at the suspend — it never consumed the next completion.
    assert provider.calls == 1
    # The pending tool call has NO tool reply (it's the dangling hole) …
    tool_msgs = [m for m in result.messages if m.role == "tool"]
    assert all(m.tool_call_id != "s1" for m in tool_msgs)
    # … but the assistant message carrying that call IS persisted.
    assert any(
        m.role == "assistant" and any(tc.id == "s1" for tc in (m.tool_calls or []))
        for m in result.messages
    )


async def test_loop_defers_only_the_suspend_call_resolving_siblings(ctx):
    # Batched assistant turn [echo, run_sandbox]: the sibling echo resolves
    # inline so the conversation has exactly one dangling tool_use.
    surf = _suspend_surface(["echo", "run_sandbox"])
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[
                    ToolCall(id="c1", name="echo", arguments={"text": "hi"}),
                    ToolCall(id="s1", name="run_sandbox", arguments={}),
                ],
            ),
        ]
    )
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surf,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=_QueueStub(),
        allow_suspend=True,
    )
    assert result.suspended is True
    assert result.pending_tool_call_id == "s1"
    tool_msgs = {m.tool_call_id: m for m in result.messages if m.role == "tool"}
    assert "c1" in tool_msgs and tool_msgs["c1"].content == "hi"  # sibling resolved
    assert "s1" not in tool_msgs  # suspend deferred


async def test_loop_ignores_suspend_without_allow_suspend(ctx):
    # Plan / Review never persist a partial conversation: a suspend result is
    # treated as a normal tool result (answered) and the loop continues.
    surf = _suspend_surface(["run_sandbox"])
    provider = _ProviderStub(
        completions=[
            Completion(
                content="",
                tool_calls=[ToolCall(id="s1", name="run_sandbox", arguments={})],
            ),
            Completion(content="final", tool_calls=[]),
        ]
    )
    result = await run_tool_loop(
        provider=provider,
        messages=[Message(role="user", content="go")],
        surface=surf,
        context=ctx,
        agent=_AgentStub(),
        max_rounds=5,
        event_queue=_QueueStub(),
    )
    assert result.suspended is False
    assert result.text == "final"
    tool_msgs = {m.tool_call_id: m for m in result.messages if m.role == "tool"}
    assert "s1" in tool_msgs  # answered, not deferred
