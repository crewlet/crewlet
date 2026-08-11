"""``OpenCodeRunner`` — OpenCode headless inside an E2B sandbox.

Runs OpenCode (preinstalled in E2B's ``opencode`` template) non-
interactively via ``opencode run`` and maps its output onto
:class:`CodingAgentResult`. OpenCode is provider-agnostic, but it resolves
``--model <provider>/<model>`` against the provider's Models.dev catalog
*and* the vendor's default endpoint. Piggy-backing the role's custom
gateway on the built-in ``openai`` provider therefore fails two ways: an
unlisted model id raises ``ProviderModelNotFoundError``, and even a listed
one would hit ``api.openai.com`` rather than the role's endpoint (the
``OPENAI_BASE_URL`` env does not redirect a built-in provider). So when the
role pins a custom ``base_url`` the runner writes a **custom provider** into
``opencode.json`` (explicit ``baseURL`` + the exact model, key via
``{env:…}``) and addresses the model under it. Its token/cost contract is
coarser than Claude Code's, so the runner records what it can (the final
text + any PR it opened) and leaves unknown numbers at zero (honest
accounting beats fabricated).

Shares the detached file plumbing with the Claude runner via
:class:`DetachedFileRunner`.
"""

from __future__ import annotations

import json
import shlex
from typing import Any

from crewlet.sandbox.coding_agents._detached import PR_RE, DetachedFileRunner
from crewlet.sandbox.protocol import (
    CodingAgentLLM,
    CodingAgentResult,
    RunLimits,
    Sandbox,
)

__all__ = [
    "OPENCODE_PROVIDER_ID",
    "OpenCodeRunner",
    "build_opencode_command",
    "opencode_model_arg",
    "opencode_stream_done",
    "parse_opencode_result",
    "to_opencode_mcp",
    "to_opencode_provider",
]

# OpenCode reads its global config here (its home in E2B's opencode
# template); writing the scoped MCP there avoids polluting the checkout.
OPENCODE_CONFIG_PATH = "/home/user/.config/opencode/opencode.json"

# The id of the custom provider we declare for the role's LLM endpoint.
# Models are then addressed as ``crewlet/<model>``.
OPENCODE_PROVIDER_ID = "crewlet"


def opencode_model_arg(llm: CodingAgentLLM | None) -> str:
    """The ``--model`` value for OpenCode given the role's LLM (or ``""``).

    With a custom ``base_url`` the model lives under our custom provider
    (``crewlet/<model>``, declared by :func:`to_opencode_provider`).
    Without one (the vendor's own endpoint) we address the matching
    built-in provider family directly (``openai/…`` / ``anthropic/…``).
    """
    if llm is None or not llm.model:
        return ""
    if llm.base_url:
        return f"{OPENCODE_PROVIDER_ID}/{llm.model}"
    family = "anthropic" if llm.provider_type == "anthropic" else "openai"
    return f"{family}/{llm.model}"


def to_opencode_provider(llm: CodingAgentLLM) -> dict[str, Any]:
    """OpenCode ``provider`` block pointing a custom provider at the role's LLM.

    Declaring the provider with an explicit ``baseURL`` + the exact model
    bypasses OpenCode's Models.dev catalog (no ``ProviderModelNotFoundError``)
    and the vendor default endpoint. The API key rides the sandbox env; we
    reference it via OpenCode's ``{env:VAR}`` interpolation so the secret is
    never written into the config payload.
    """
    anthropic = llm.provider_type == "anthropic"
    npm = "@ai-sdk/anthropic" if anthropic else "@ai-sdk/openai-compatible"
    key_env = "ANTHROPIC_API_KEY" if anthropic else "OPENAI_API_KEY"
    return {
        OPENCODE_PROVIDER_ID: {
            "npm": npm,
            "name": "Crewlet role LLM",
            "options": {
                "baseURL": llm.base_url,
                "apiKey": f"{{env:{key_env}}}",
            },
            "models": {llm.model: {}},
        }
    }


def to_opencode_mcp(mcp_servers: dict[str, dict[str, Any]]) -> dict[str, Any]:
    """Translate the generic launch specs into OpenCode's ``mcp`` schema.

    stdio → ``{"type": "local", "command": [...], "environment": {...}}``;
    http  → ``{"type": "remote", "url": ..., "headers": {...}}``. Every
    entry is ``enabled``.
    """
    out: dict[str, Any] = {}
    for name, spec in mcp_servers.items():
        if spec.get("type") == "http":
            entry: dict[str, Any] = {
                "type": "remote",
                "url": spec.get("url", ""),
                "enabled": True,
            }
            if spec.get("headers"):
                entry["headers"] = spec["headers"]
        else:
            cmd = [spec.get("command", ""), *spec.get("args", [])]
            entry = {
                "type": "local",
                "command": [c for c in cmd if c],
                "enabled": True,
            }
            if spec.get("env"):
                entry["environment"] = spec["env"]
        out[name] = entry
    return out


def build_opencode_command(
    brief: str, *, model: str = "", limits: RunLimits | None = None
) -> str:
    """Build the non-interactive ``opencode run`` invocation.

    Always ``--format json``: OpenCode then streams newline-delimited events
    to stdout, **flushed per line as they happen**. That is essential here —
    ``opencode run`` is known to finish its work but never exit (it leaves
    handles open), and in default mode its final summary is buffered and lost
    when the process hangs. With ``--format json`` the assistant text and a
    terminal event (a ``step_finish`` with reason ``stop``) are written live,
    so the result is captured and completion is detectable even though the
    process hangs (see :func:`parse_opencode_result` /
    :func:`opencode_stream_done`).

    ``model`` is the fully-formed ``--model`` value (``<provider>/<model>``,
    see :func:`opencode_model_arg`), passed only when the role pinned one.
    Pure + unit-pinned.
    """
    parts = ["opencode", "run", shlex.quote(brief)]
    if model:
        parts += ["--model", shlex.quote(model)]
    parts += ["--format", "json"]
    return " ".join(parts)


def _iter_events(stdout: str) -> list[dict[str, Any]]:
    """Parse ``opencode --format json`` newline-delimited events.

    Each line is one JSON object ``{type, timestamp, sessionID, ...data}``.
    Non-JSON / partial lines are skipped (a SIGKILLed run can leave a half-
    written final line). Returns the decoded event dicts in order.
    """
    events: list[dict[str, Any]] = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            obj = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        if isinstance(obj, dict) and obj.get("type"):
            events.append(obj)
    return events


def opencode_stream_done(stdout: str) -> bool:
    """True once the streamed events carry the terminal completion signal.

    ``opencode run --format json`` writes its events live (flushed per line),
    so the terminal lands even though the process then hangs without exiting.
    The terminal, in order of what real versions emit:

    - a ``step_finish`` whose ``part.reason == "stop"`` — the assistant
      produced its final message and made no further tool calls (v1.17.x;
      intermediate steps carry ``reason == "tool-calls"``);
    - a ``session.status`` event with ``status.type == "idle"`` (newer
      builds);
    - an ``error`` event.

    Parses the stream (we already hold the bytes from the poll read), so it
    is tolerant of JSON spacing.
    """
    for obj in _iter_events(stdout):
        etype = obj.get("type")
        if etype == "error":
            return True
        part = obj.get("part") if isinstance(obj.get("part"), dict) else {}
        if etype == "step_finish" and part.get("reason") == "stop":
            return True
        if etype == "session.status":
            status = (obj.get("properties") or {}).get("status") or {}
            if isinstance(status, dict) and status.get("type") == "idle":
                return True
    return False


def _tool_transcript_line(part: dict[str, Any], obj: dict[str, Any]) -> str:
    """One transcript line for a tool_use event, enriched for diagnosis.

    A bare ``[tool] bash`` list is useless when a sandbox run fails — you
    can't see WHAT ran. OpenCode's tool events carry the call's input (the
    bash command, the file path, …) and a state with status/error, though the
    exact nesting varies across versions, so we probe defensively and fall
    back to just the name. Used only for the dashboard transcript; never
    fed to the LLM, so a little extra length is fine, but we still cap the
    command echo so a giant heredoc can't blow up the phase event.
    """
    name = part.get("tool") or obj.get("tool") or obj.get("name") or "tool"
    state = part.get("state") if isinstance(part.get("state"), dict) else {}
    inp = state.get("input")
    if not isinstance(inp, dict):
        inp = part.get("input") if isinstance(part.get("input"), dict) else {}

    detail = ""
    if isinstance(inp, dict) and inp:
        raw = (
            inp.get("command")
            or inp.get("filePath")
            or inp.get("path")
            or inp.get("pattern")
            or inp.get("description")
            or ""
        )
        detail = str(raw).strip().splitlines()[0] if raw else ""
        if len(detail) > 160:
            detail = detail[:157] + "…"

    line = f"[tool] {name}" + (f": {detail}" if detail else "")

    status = state.get("status")
    err = state.get("error") or (state.get("output") if status == "error" else "")
    if status == "error" or err:
        err_txt = str(err).strip().splitlines()[0] if err else "failed"
        if len(err_txt) > 160:
            err_txt = err_txt[:157] + "…"
        line += f" → error: {err_txt}"
    return line


def parse_opencode_result(stdout: str) -> CodingAgentResult:
    """Map ``opencode run --format json`` output onto a result.

    Reconstructs the assistant's answer from the ``text`` events
    (``.part.text``) and a readable activity transcript from the tool / text
    events; surfaces an ``error`` event as a failure. OpenCode exposes no
    stable token/cost envelope, so those stay zero (honest accounting).
    Falls back to treating the whole blob as plain text for a non-JSON run
    (older OpenCode, or output captured before ``--format json``).
    """
    text = (stdout or "").strip()
    if not text:
        return CodingAgentResult(success=False, error="empty coding-agent output")

    events = _iter_events(text)
    if not events:
        # Non-JSON output (older OpenCode / plain run) — keep the old shape.
        return CodingAgentResult(
            text=text, success=True, delivered_refs=PR_RE.findall(text)
        )

    answers: list[str] = []
    transcript: list[str] = []
    error = ""
    for obj in events:
        etype = obj.get("type")
        part = obj.get("part") if isinstance(obj.get("part"), dict) else {}
        if etype == "text":
            chunk = str(part.get("text", "")).strip()
            if chunk:
                answers.append(chunk)
                transcript.append(chunk)
        elif etype == "tool_use":
            transcript.append(_tool_transcript_line(part, obj))
        elif etype == "error":
            err = obj.get("error")
            error = err if isinstance(err, str) else json.dumps(err)
            transcript.append(f"[error] {error}")
    body = "\n".join(answers).strip()
    success = bool(body) and not error
    return CodingAgentResult(
        text=body,
        success=success,
        transcript="\n".join(transcript).strip(),
        delivered_refs=PR_RE.findall(body),
        error="" if success else (error or "no assistant output"),
    )


class OpenCodeRunner(DetachedFileRunner):
    """Runs OpenCode headless in a sandbox; returns a CodingAgentResult."""

    name = "opencode"

    def _build_command(
        self,
        brief: str,
        limits: RunLimits,
        *,
        llm: CodingAgentLLM | None = None,
        mcp_config_path: str = "",
    ) -> str:
        # OpenCode auto-loads its global ``opencode.json`` (written by
        # ``_write_config``); it has no ``--mcp-config`` flag, so MCP is
        # config-driven, not CLI-driven. The model IS a CLI arg
        # (``<provider>/<model>``) -- without it OpenCode uses its built-in
        # default instead of the role's LLM.
        return build_opencode_command(
            brief, model=opencode_model_arg(llm), limits=limits
        )

    async def _write_config(
        self,
        sandbox: Sandbox,
        mcp_servers: dict[str, Any],
        llm: CodingAgentLLM | None = None,
    ) -> str:
        # OpenCode auto-loads this global config: it carries both the scoped
        # MCP surface and the custom provider that points OpenCode at the
        # role's LLM endpoint + model (only when a custom base_url is set --
        # otherwise the built-in provider family is used directly).
        #
        # ``permission: "allow"`` is the OpenCode equivalent of Claude Code's
        # ``--permission-mode bypassPermissions``: a single blanket string
        # grants every permission type (edit, bash, webfetch, ``read``, and
        # crucially ``external_directory``). Headless ``opencode run`` has no
        # human to answer a permission prompt, so any gate left at the default
        # ``ask`` is AUTO-REJECTED — which is what silently killed runs whose
        # checkout lives outside OpenCode's cwd (the repo cloned under ``/tmp``
        # trips ``external_directory``). The sandbox is the isolation boundary,
        # so the in-box agent is trusted with full tool access exactly like
        # Claude Code.
        #
        # ``share: disabled`` + ``autoupdate: false`` are headless hygiene:
        # a session-share connection can keep the process alive past the run
        # (a cause of the detached job never exiting) AND would publish the
        # session to OpenCode's cloud from inside the sandbox; self-update
        # mid-job is likewise unwanted. Always written, even with no
        # provider/MCP, so these defaults always apply.
        config: dict[str, Any] = {
            "$schema": "https://opencode.ai/config.json",
            "permission": "allow",
            "share": "disabled",
            "autoupdate": False,
        }
        if llm is not None and llm.base_url and llm.model:
            config["provider"] = to_opencode_provider(llm)
        if mcp_servers:
            config["mcp"] = to_opencode_mcp(mcp_servers)
        await sandbox.exec("mkdir -p /home/user/.config/opencode")
        await sandbox.write_file(OPENCODE_CONFIG_PATH, json.dumps(config, indent=2))
        return ""  # auto-loaded; not passed on the CLI

    def _parse_result(self, stdout: str) -> CodingAgentResult:
        return parse_opencode_result(stdout)

    def _result_done(self, stdout: str) -> bool:
        # `opencode run` finishes its work but never exits (known bug), so the
        # shell never writes the done-marker. Completion is the terminal event
        # in the streamed output — a `step_finish` with reason "stop" (or a
        # `session.status: idle` / `error` event); see opencode_stream_done.
        return opencode_stream_done(stdout)
