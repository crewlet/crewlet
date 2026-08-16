"""Wire format between Crewlet's tool loop and a coding CLI.

A coding CLI has no ``tools=[...]`` parameter and no structured
tool-call channel: it takes one prompt string and prints one answer.
Crewlet's tool loop needs the opposite — a message list in, an assistant
message plus zero or more tool calls out.  This module is the adapter,
and it is deliberately a *pure*, dependency-free pair of functions so
every shape it has to survive is unit-pinned:

- :func:`render_prompt` flattens the phase transcript, the tool
  definitions and the response contract into one prompt.
- :func:`parse_envelope` reads the model's reply back into content +
  tool calls.

**Why a JSON envelope and not the CLI's own tool system.**  Every one of
these CLIs *does* have tools — file edits, shell, web fetch — but they
are the CLI's tools, running in the CLI's sandbox, invisible to
Crewlet's registry, its permission model, its secret redaction and its
event stream.  Routing agent work through them would fork the engine's
tool surface in two.  So the CLI is used strictly as a text model and
the tool channel is carried in-band.

**Degrading is safe.**  When a reply carries no parseable envelope the
whole reply becomes assistant content with no tool calls, which the tool
loop already handles: a phase that needed a call re-prompts through the
existing ``tool_choice="required"`` corrective path
(:data:`crewlet.agent.llm_loop._MAX_FORCED_TOOL_RETRIES`) instead of
crashing.  Nothing here raises on bad model output.
"""

from __future__ import annotations

import json
import re
from typing import Any

from crewlet._logging import get_logger
from crewlet.providers.llm.protocol import Message, ToolCall, ToolDef

logger = get_logger("llm.cli.protocol")

#: Fenced blocks the model may wrap the envelope in.
_FENCE = re.compile(
    r"```(?:json|jsonc|json5)?[ \t]*\r?\n(.*?)```", re.DOTALL | re.IGNORECASE
)

#: Keys accepted for the assistant's visible text, in priority order.
#: Models drift between these three no matter how the contract is
#: worded, and rejecting a reply over the synonym it picked would fail
#: a turn for nothing.
_TEXT_KEYS = ("message", "content", "text", "response")

_ENVELOPE_EXAMPLE = json.dumps(
    {
        "message": "Short note to the operator, or an empty string.",
        "tool_calls": [{"name": "tool_name", "arguments": {"arg": "value"}}],
    },
    indent=2,
)


def render_tool_catalogue(tools: list[ToolDef]) -> str:
    """The callable-tool block: name, description, JSON Schema."""
    entries = [
        {
            "name": t.name,
            "description": t.description,
            "parameters": t.parameters or {"type": "object", "properties": {}},
        }
        for t in tools
    ]
    return json.dumps(entries, indent=2, ensure_ascii=False)


def render_contract(tools: list[ToolDef], tool_choice: str | None) -> str:
    """The response contract appended after the tool catalogue."""
    names = ", ".join(f"`{t.name}`" for t in tools)
    must = (
        "You MUST include at least one entry in `tool_calls` this turn."
        if tool_choice == "required"
        else "Leave `tool_calls` empty when no tool is needed."
    )
    return (
        "# Response format\n"
        "You are being driven by an automation harness, not a human. Your "
        "entire reply must be ONE fenced JSON block and nothing else — no "
        "preamble, no explanation before or after the fence, no second "
        "block.\n\n"
        "```json\n"
        f"{_ENVELOPE_EXAMPLE}\n"
        "```\n\n"
        f"- `name` must be exactly one of: {names}.\n"
        "- `arguments` must be a JSON object matching that tool's "
        "`parameters` schema. Never a string, never omitted.\n"
        f"- {must}\n"
        "- Put any prose for the operator in `message`; it is the only "
        "field that is read as text.\n"
        "- Do not use your own file, shell, or web tools. The tools listed "
        "above are the only ones that exist, and they run outside this "
        "process — request them through `tool_calls` and you will receive "
        "their results on the next turn."
    )


def _render_message(msg: Message) -> str:
    """One transcript entry, labelled by role."""
    if msg.role == "tool":
        label = f"## Tool result — {msg.name or msg.tool_call_id or 'tool'}"
        return f"{label}\n{msg.content}"
    if msg.role == "assistant":
        parts = []
        if msg.content:
            parts.append(msg.content)
        for call in msg.tool_calls:
            args = json.dumps(call.arguments, ensure_ascii=False, default=str)
            parts.append(f"[called {call.name} with {args}]")
        return "## Assistant\n" + ("\n".join(parts) or "(no output)")
    if msg.role == "system":
        return msg.content
    return f"## {msg.role.capitalize()}\n{msg.content}"


def render_prompt(
    messages: list[Message],
    tools: list[ToolDef] | None = None,
    tool_choice: str | None = None,
) -> str:
    """Flatten a phase's messages + tools into one CLI prompt.

    System messages are hoisted to the front verbatim (they are the
    phase's instructions and every CLI treats leading text as such);
    the rest render as a labelled transcript.  When ``tools`` is empty
    no contract is added at all — an auxiliary call that just wants
    prose gets a plain prompt and a plain answer, with no envelope to
    get wrong.
    """
    system = [m.content for m in messages if m.role == "system" and m.content]
    rest = [m for m in messages if m.role != "system"]

    sections: list[str] = []
    if system:
        sections.append("\n\n".join(system))
    if rest:
        sections.append(
            "# Conversation so far\n\n" + "\n\n".join(_render_message(m) for m in rest)
        )
    if tools:
        sections.append("# Tools you can call\n\n" + render_tool_catalogue(tools))
        sections.append(render_contract(tools, tool_choice))
    return "\n\n".join(s for s in sections if s.strip())


def _iter_json_candidates(text: str) -> list[str]:
    """Every plausible JSON envelope in ``text``, best candidate first.

    Fenced blocks come first (the contract asks for one), last fence
    before earlier fences — a model that "shows its work" puts the real
    answer last.  Bare balanced ``{...}`` spans are the fallback for a
    model that dropped the fence.
    """
    candidates = [m.group(1).strip() for m in _FENCE.finditer(text)]
    candidates.reverse()

    # Bare objects: scan for balanced braces outside string literals.
    depth = 0
    start = -1
    in_string = False
    escaped = False
    bare: list[str] = []
    for i, ch in enumerate(text):
        if in_string:
            if escaped:
                escaped = False
            elif ch == "\\":
                escaped = True
            elif ch == '"':
                in_string = False
            continue
        if ch == '"':
            in_string = True
        elif ch == "{":
            if depth == 0:
                start = i
            depth += 1
        elif ch == "}" and depth > 0:
            depth -= 1
            if depth == 0 and start >= 0:
                bare.append(text[start : i + 1])
    bare.reverse()
    candidates.extend(bare)

    seen: set[str] = set()
    unique: list[str] = []
    for c in candidates:
        if c and c not in seen:
            seen.add(c)
            unique.append(c)
    return unique


def _coerce_arguments(raw: Any) -> dict[str, Any]:
    """Normalise a tool call's ``arguments`` into a dict.

    Models routinely emit the arguments as a JSON *string* (the OpenAI
    function-calling wire shape leaking through). Accepting both costs
    three lines and saves a wasted round.
    """
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        stripped = raw.strip()
        if not stripped:
            return {}
        try:
            parsed = json.loads(stripped)
        except (json.JSONDecodeError, ValueError):
            return {}
        return parsed if isinstance(parsed, dict) else {}
    return {}


def _extract_tool_calls(payload: dict[str, Any], id_prefix: str) -> list[ToolCall]:
    raw_calls = payload.get("tool_calls")
    if isinstance(raw_calls, dict):
        # A single call written as an object instead of a one-element
        # list — common enough to accept rather than discard.
        raw_calls = [raw_calls]
    if not isinstance(raw_calls, list):
        return []
    calls: list[ToolCall] = []
    for index, entry in enumerate(raw_calls):
        if not isinstance(entry, dict):
            continue
        name = str(entry.get("name") or entry.get("tool") or "").strip()
        if not name:
            continue
        arguments = entry.get("arguments")
        if arguments is None:
            arguments = entry.get("parameters", entry.get("input"))
        calls.append(
            ToolCall(
                id=str(entry.get("id") or f"{id_prefix}_{index}"),
                name=name,
                arguments=_coerce_arguments(arguments),
            )
        )
    return calls


def _looks_like_envelope(payload: dict[str, Any]) -> bool:
    """Whether ``payload`` is our envelope rather than incidental JSON.

    A reply can legitimately contain a JSON *example* the model was
    asked about; treating that as the envelope would swallow the real
    answer.  Requiring one of the envelope's own keys keeps the bare-
    brace fallback from firing on unrelated objects.
    """
    return "tool_calls" in payload or any(k in payload for k in _TEXT_KEYS)


def parse_envelope(text: str, *, id_prefix: str = "call") -> tuple[str, list[ToolCall]]:
    """Read ``(content, tool_calls)` out of a CLI's answer.

    Never raises.  When no envelope is found the whole reply is the
    content and the tool-call list is empty — the tool loop treats that
    as a normal no-tool-call round.
    """
    raw = (text or "").strip()
    if not raw:
        return "", []
    for candidate in _iter_json_candidates(raw):
        try:
            payload = json.loads(candidate)
        except (json.JSONDecodeError, ValueError):
            continue
        if not isinstance(payload, dict) or not _looks_like_envelope(payload):
            continue
        content = ""
        for key in _TEXT_KEYS:
            value = payload.get(key)
            if isinstance(value, str) and value.strip():
                content = value
                break
            if value is not None:
                content = json.dumps(value, ensure_ascii=False, default=str)
                break
        calls = _extract_tool_calls(payload, id_prefix)
        if content or calls:
            return content, calls
        # An envelope that parsed but carried neither text nor a call
        # says nothing; keep looking before falling back to raw text.
    logger.debug("cli_envelope_absent", chars=len(raw))
    return raw, []


def json_path(payload: Any, path: list[str]) -> Any:
    """Follow a declarative profile path into decoded CLI output."""
    node = payload
    for key in path:
        if not isinstance(node, dict):
            return None
        node = node.get(key)
        if node is None:
            return None
    return node


def estimate_tokens(text: str) -> int:
    """Rough token count for a CLI that reports no usage.

    Four characters per token is the long-standing English-text
    approximation, and an approximation is the honest option here: the
    budget cascade and the ``token_usage`` ledger must keep *moving* for
    a subscription backend, or an unmetered seat would silently run
    without a ceiling.  ``crewlet llm doctor`` reports whether a profile
    yields real usage numbers or estimates.
    """
    return max(1, len(text or "") // 4)


__all__ = [
    "estimate_tokens",
    "json_path",
    "parse_envelope",
    "render_contract",
    "render_prompt",
    "render_tool_catalogue",
]
