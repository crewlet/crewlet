"""``CLIAgentProvider`` — a locally installed coding CLI as an LLM backend.

Lets a role run on a **subscription** (Claude Pro/Max, ChatGPT Plus/Pro,
Google AI Pro, a Copilot or Cursor seat) instead of a metered API key,
by driving the vendor's own CLI — ``claude``, ``codex``, ``gemini``,
``opencode``, … — as a headless text model.  The CLI already holds the
operator's OAuth login; Crewlet never sees a password and never
re-implements a vendor's auth.

Three things make this safe to put behind
:class:`~crewlet.providers.llm.protocol.LLMProvider`:

**Isolation.**  Every call runs in a per-seat home
(:mod:`crewlet.providers.llm.cli_workspace`) with an allowlisted
environment, a throwaway working directory, and the CLI's conversation
state wiped around it.  Seven seats on one subscription do not read each
other's transcripts, and no turn inherits the last one's session.

**Tool calls.**  The tool loop's ``tools=[...]`` are rendered into the
prompt and read back out of a JSON envelope
(:mod:`crewlet.providers.llm.cli_protocol`) — the CLI's *own* tools stay
out of it, so Crewlet's registry, permission model and secret redaction
remain the only tool surface.

**Composability.**  A spent subscription window is reported as
``RATE_LIMIT``, so the standard fallback chain
(``llm: [claude-cli, anthropic]``) carries the role onto a metered key
until the window resets, with no operator intervention.

See ``docs/concepts/subscription-llm-backends.md``.
"""

from __future__ import annotations

import asyncio
import os
import re
import shutil
import signal
import sys
from collections.abc import AsyncIterator
from pathlib import Path
from typing import Any, Literal

from crewlet._logging import get_logger
from crewlet.providers.errors import ProviderCallError, ProviderErrorKind
from crewlet.providers.llm.cli_profiles import (
    HOME_PLACEHOLDER,
    MODEL_PLACEHOLDER,
    PROMPT_PLACEHOLDER,
    CLIAgentProfile,
)
from crewlet.providers.llm.cli_protocol import (
    estimate_tokens,
    json_path,
    parse_envelope,
    render_prompt,
)
from crewlet.providers.llm.cli_workspace import CLIWorkspaceManager, SeatWorkspace
from crewlet.providers.llm.protocol import (
    Completion,
    CompletionChunk,
    Message,
    ToolDef,
)
from crewlet.providers.llm.scope import current_scope
from crewlet.secrets.resolver import resolve_env

logger = get_logger("llm.cli")

AuthMode = Literal["subscription", "api-key", "inherit-env"]

#: Seconds a CLI process gets to shut down after SIGTERM before it is
#: SIGKILLed.  Long enough for a Node runtime to flush its output and
#: run exit handlers; short enough that a wedged child cannot hold the
#: turn open past the caller's own deadline.
_TERM_GRACE_SECONDS = 5.0

#: How much of a failed run's output is carried in the raised error.
#: Enough for the operator to see the vendor's message in the log
#: without pasting a whole transcript into every event payload.
_ERROR_EXCERPT_CHARS = 2000

#: How long a reply may be and still be read as a usage-limit notice.
#:
#: Only consulted where the model had no tool surface, so there is no
#: envelope to settle it structurally — see
#: :meth:`CLIAgentProvider._looks_like_a_usage_limit`. A vendor's
#: message is a sentence or two ("Usage limit reached. Resets at 4pm",
#: and the most verbose shipped form adds a reset timestamp and an
#: upgrade line — still under 200 characters), while an answer that
#: merely mentions rate limiting is substantive prose. 500 leaves that
#: 2.5x of headroom without reaching into the length real answers start
#: at.
_LIMIT_MESSAGE_MAX_CHARS = 500


class CLIAgentConfigError(ValueError):
    """The CLI backend cannot be constructed as configured."""


def _substitute(args: list[str], *, model: str, prompt: str, home: str) -> list[str]:
    """Fill the profile's argv placeholders."""
    out: list[str] = []
    for arg in args:
        out.append(
            arg.replace(MODEL_PLACEHOLDER, model)
            .replace(PROMPT_PLACEHOLDER, prompt)
            .replace(HOME_PLACEHOLDER, home)
        )
    return out


def _compile(patterns: list[str]) -> list[re.Pattern[str]]:
    compiled: list[re.Pattern[str]] = []
    for raw in patterns:
        try:
            compiled.append(re.compile(raw, re.IGNORECASE))
        except re.error:
            logger.warning("cli_pattern_invalid", pattern=raw)
    return compiled


class CLIAgentProvider:
    """Drives a coding CLI as a Crewlet LLM provider."""

    def __init__(
        self,
        *,
        provider_key: str,
        profile: CLIAgentProfile,
        model: str = "",
        state_dir: str = "",
        timeout: float = 300.0,
        max_concurrent: int = 4,
        auth_mode: AuthMode = "subscription",
        api_key: str = "",
        subscription_token: str = "",
        env: dict[str, str] | None = None,
        require_binary: bool = True,
    ) -> None:
        if not profile.binary:
            raise CLIAgentConfigError(
                f"LLM provider {provider_key!r} (type 'cli-agent') has no "
                "binary: set providers.llm."
                f"{provider_key}.cli.overrides.binary. The 'custom' profile "
                "ships none by design."
            )
        if not profile.complete_args and profile.prompt_via == "argv":
            raise CLIAgentConfigError(
                f"LLM provider {provider_key!r} declares prompt_via 'argv' but "
                "no complete_args, so the prompt has nowhere to go. Add "
                f"providers.llm.{provider_key}.cli.overrides.complete_args "
                f"containing {PROMPT_PLACEHOLDER!r}."
            )

        self.provider_key = provider_key
        self.profile = profile
        self.model = model or profile.name
        self._timeout = timeout
        self._auth_mode: AuthMode = auth_mode
        self._api_key = api_key
        self._subscription_token = subscription_token
        self._rate_limit = _compile(profile.rate_limit_patterns)
        self._auth_errors = _compile(profile.auth_error_patterns)
        # Coding CLIs are heavyweight processes (a Node runtime at
        # 200-400 MB RSS each), and subscription plans throttle
        # concurrency far below what an API key allows. Cap how many run
        # at once so a fleet of seats hitting Plan simultaneously
        # doesn't exhaust the engine host's memory.
        self._slots = asyncio.Semaphore(max(1, int(max_concurrent)))
        self._workspace = CLIWorkspaceManager(
            provider_key=provider_key,
            profile=profile,
            state_dir=Path(state_dir).expanduser() if state_dir else None,
            extra_env=self._auth_env(env or {}),
        )
        self._binary = shutil.which(profile.binary) or profile.binary
        if require_binary and not shutil.which(profile.binary):
            raise CLIAgentConfigError(
                f"LLM provider {provider_key!r} (type 'cli-agent', agent "
                f"{profile.name!r}) needs the {profile.binary!r} command on "
                "PATH of the process running `crewlet run`, and it is not "
                "there. Install the CLI on the engine host, or point "
                f"providers.llm.{provider_key}.cli.overrides.binary at its "
                "absolute path."
            )
        self._children: set[asyncio.subprocess.Process] = set()
        self._closed = False

    # -- properties the engine reads ---------------------------------

    @property
    def call_timeout_seconds(self) -> float:
        """Advisory floor for callers that impose their own deadline.

        The auxiliary-LLM helper caps a single call at 60 s, which is
        the right ceiling for an HTTP round trip and far too tight for a
        process launch plus a model call. Exposing the transport's real
        budget lets that caller widen its deadline instead of timing out
        every prefetch on this backend.
        """
        return self._timeout

    @property
    def workspace(self) -> CLIWorkspaceManager:
        """The provider's isolation manager (used by ``crewlet llm``)."""
        return self._workspace

    # -- auth --------------------------------------------------------

    def _auth_env(self, operator_env: dict[str, str]) -> dict[str, str]:
        """Credential environment for every child process.

        Deliberately *not* inherited from the engine's environment.  A
        subscription backend that picked up a stray ``ANTHROPIC_API_KEY``
        would quietly bill the metered account while the operator
        believed they were on a flat-rate plan — the exact failure this
        feature exists to avoid — so ``subscription`` mode passes only
        the subscription token, and an API key reaches the child only
        when ``auth.mode`` asks for one.
        """
        env = dict(operator_env)
        profile = self.profile
        if self._auth_mode == "subscription":
            if profile.token_env and self._subscription_token:
                env.setdefault(profile.token_env, self._subscription_token)
        elif self._auth_mode == "api-key":
            if not profile.api_key_env:
                raise CLIAgentConfigError(
                    f"LLM provider {self.provider_key!r} sets auth.mode "
                    f"'api-key' but the {profile.name!r} profile declares no "
                    "api_key_env. Override it, or use auth.mode "
                    "'subscription'."
                )
            if not self._api_key:
                raise CLIAgentConfigError(
                    f"LLM provider {self.provider_key!r} sets auth.mode "
                    "'api-key' but no key is reachable: set "
                    f"providers.llm.{self.provider_key}.api_keys, or store "
                    f"{profile.api_key_env} in the secret store."
                )
            env.setdefault(profile.api_key_env, self._api_key)
        else:  # inherit-env
            for name in (profile.token_env, profile.api_key_env):
                if not name:
                    continue
                value = resolve_env(name)
                if value:
                    env.setdefault(name, value)
        return env

    # -- process plumbing --------------------------------------------

    def _build_argv(self, prompt: str, workspace: SeatWorkspace) -> list[str]:
        args = list(self.profile.complete_args)
        if self.model and self.profile.model_args:
            args += self.profile.model_args
        argv = [self._binary, *args]
        if self.profile.prompt_via == "argv" and PROMPT_PLACEHOLDER not in " ".join(
            args
        ):
            argv.append(PROMPT_PLACEHOLDER)
        return _substitute(
            argv,
            model=self.model,
            prompt=prompt if self.profile.prompt_via == "argv" else "",
            home=str(workspace.home),
        )

    async def _terminate(self, proc: asyncio.subprocess.Process) -> None:
        """Kill a timed-out CLI and everything it spawned.

        The child is started in its own session, so signalling the
        process *group* reaches the Node runtime's helpers too; killing
        only the parent leaves them holding the seat's home open and the
        next generation's prune racing live writers.
        """
        if proc.returncode is not None:
            return
        try:
            if sys.platform == "win32":  # pragma: no cover — POSIX in CI
                proc.terminate()
            else:
                os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
        except (ProcessLookupError, PermissionError, OSError):
            return
        try:
            await asyncio.wait_for(proc.wait(), timeout=_TERM_GRACE_SECONDS)
            return
        except TimeoutError:
            pass
        try:
            if sys.platform == "win32":  # pragma: no cover — POSIX in CI
                proc.kill()
            else:
                os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except (ProcessLookupError, PermissionError, OSError):
            return
        try:
            await asyncio.wait_for(proc.wait(), timeout=_TERM_GRACE_SECONDS)
        except TimeoutError:  # pragma: no cover
            logger.error("cli_process_unkillable", pid=proc.pid)

    async def _run(
        self, argv: list[str], stdin_text: str, workspace: SeatWorkspace
    ) -> tuple[int, str, str]:
        """Run one CLI invocation; return ``(exit_code, stdout, stderr)``."""
        kwargs: dict[str, Any] = {}
        if sys.platform != "win32":
            # Own session => own process group => the whole tree is
            # reachable by one killpg on timeout.
            kwargs["start_new_session"] = True
        proc = await asyncio.create_subprocess_exec(
            *argv,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=str(workspace.work),
            env=workspace.env,
            **kwargs,
        )
        self._children.add(proc)
        try:
            payload = stdin_text.encode("utf-8") if stdin_text else b""
            try:
                stdout, stderr = await asyncio.wait_for(
                    proc.communicate(input=payload), timeout=self._timeout
                )
            except TimeoutError as exc:
                await self._terminate(proc)
                raise ProviderCallError(
                    f"{self.profile.binary} did not answer within {self._timeout:g}s",
                    ProviderErrorKind.TIMEOUT,
                    provider_key=self.provider_key,
                ) from exc
            except asyncio.CancelledError:
                # Turn cancelled (shutdown, deadline): never leave the
                # child running against the seat's home.
                await self._terminate(proc)
                raise
        finally:
            self._children.discard(proc)
        return (
            proc.returncode or 0,
            stdout.decode("utf-8", errors="replace"),
            stderr.decode("utf-8", errors="replace"),
        )

    # -- output handling ---------------------------------------------

    def _decode(self, stdout: str) -> tuple[str, dict[str, int]]:
        """Extract the answer text and token usage from CLI output."""
        import json as _json

        profile = self.profile
        usage: dict[str, int] = {}

        def _absorb(payload: Any) -> None:
            for field, paths in (
                ("input", profile.input_token_paths),
                ("output", profile.output_token_paths),
                ("cache_read", profile.cache_read_token_paths),
                ("cache_write", profile.cache_write_token_paths),
            ):
                for path in paths:
                    value = json_path(payload, path)
                    if isinstance(value, int | float) and not isinstance(value, bool):
                        usage[field] = int(value)
                        break

        def _text_of(payload: Any) -> str:
            for path in profile.text_paths:
                value = json_path(payload, path)
                if isinstance(value, str) and value:
                    return value
            return ""

        if profile.output_format == "text":
            return stdout.strip(), usage

        if profile.output_format == "json":
            payload = _loads_tolerant(stdout)
            if payload is None:
                # Not JSON at all: the CLI printed a plain message (a
                # crash banner, a login prompt). Surfacing it verbatim
                # beats swallowing it — the caller classifies it.
                return stdout.strip(), usage
            _absorb(payload)
            text = _text_of(payload)
            for path in profile.error_paths:
                err = json_path(payload, path)
                if isinstance(err, str) and err.strip():
                    return text or err, usage
            return text or stdout.strip(), usage

        # jsonl: one event per line.
        chunks: list[str] = []
        for line in stdout.splitlines():
            line = line.strip()
            if not line or not line.startswith("{"):
                continue
            try:
                payload = _json.loads(line)
            except (ValueError, TypeError):
                continue
            _absorb(payload)
            text = _text_of(payload)
            if text:
                chunks.append(text)
        if not chunks:
            return stdout.strip(), usage
        if profile.jsonl_text_mode == "concat":
            return "".join(chunks), usage
        return chunks[-1], usage

    def _classify_output(self, combined: str) -> ProviderErrorKind:
        """Map a failed run's output onto a provider error class."""
        for pattern in self._rate_limit:
            if pattern.search(combined):
                return ProviderErrorKind.RATE_LIMIT
        for pattern in self._auth_errors:
            if pattern.search(combined):
                return ProviderErrorKind.AUTH
        return ProviderErrorKind.FATAL

    # -- LLMProvider surface -----------------------------------------

    def _looks_like_a_usage_limit(self, text: str) -> bool:
        """Whether a zero-exit reply is the CLI saying its window is spent.

        Reached only when no tool call was parsed. With a tool surface
        that already means the CLI ignored the response contract, which
        the model answering the turn does not do — so the wording is
        enough. Without one there was no contract to violate, and the
        only remaining signal is that a vendor's limit notice is SHORT
        and is the whole reply, where an answer that happens to discuss
        rate limiting is not.
        """
        stripped = text.strip()
        if not stripped or len(stripped) > _LIMIT_MESSAGE_MAX_CHARS:
            return False
        return any(pattern.search(stripped) for pattern in self._rate_limit)

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice: str | None = None,
    ) -> Completion:
        """Run one CLI invocation and map its answer onto a completion.

        ``temperature`` and ``max_tokens`` are accepted for protocol
        compatibility and ignored: a subscription CLI exposes neither
        (both are the vendor's own per-plan defaults). Silently
        accepting them is correct here — the caller passes the tool
        loop's defaults on every call, and refusing would make the
        backend unusable for no gain.
        """
        if self._closed:
            raise ProviderCallError(
                f"LLM provider {self.provider_key!r} is closed",
                ProviderErrorKind.FATAL,
                provider_key=self.provider_key,
            )
        scope = current_scope()
        prompt = render_prompt(messages, tools, tool_choice)

        async with self._slots, self._workspace.acquire(scope.seat) as workspace:
            argv = self._build_argv(prompt, workspace)
            stdin_text = prompt if self.profile.prompt_via == "stdin" else ""
            logger.debug(
                "cli_llm_request",
                provider=self.provider_key,
                agent=self.profile.name,
                seat=workspace.seat,
                phase=scope.phase,
                model=self.model,
                prompt_chars=len(prompt),
                tool_count=len(tools or []),
            )
            exit_code, stdout, stderr = await self._run(argv, stdin_text, workspace)

        text, usage = self._decode(stdout)
        combined = f"{text}\n{stderr}"

        if exit_code != 0 or (not text.strip() and stderr.strip()):
            kind = self._classify_output(combined)
            excerpt = (stderr.strip() or text.strip())[:_ERROR_EXCERPT_CHARS]
            logger.warning(
                "cli_llm_failed",
                provider=self.provider_key,
                agent=self.profile.name,
                seat=workspace.seat,
                exit_code=exit_code,
                kind=kind.value,
            )
            raise ProviderCallError(
                f"{self.profile.binary} exited {exit_code}: {excerpt}",
                kind,
                provider_key=self.provider_key,
            )

        if tools:
            content, tool_calls = parse_envelope(text, id_prefix=self.provider_key)
        else:
            # No tool surface means no response contract went out, so
            # there is no envelope to look for — the reply IS the answer.
            content, tool_calls = text, []

        # A zero exit with limit wording is how these CLIs report a spent
        # subscription window; without this the phase would consume the
        # apology as the model's answer instead of falling through to
        # the next provider in the role's chain.
        #
        # But the wording is ordinary engineering prose — "rate limit",
        # "429", "overloaded", "too many requests", "resets at" — so
        # matching it against the model's ANSWER threw away real work.
        # An agent asked to add rate limiting to a gateway had its plan
        # discarded as a provider outage, on the fastest path to a
        # metered bill (RATE_LIMIT is retryable, so a chain falls
        # through to the paid key) or, on a subscription-only chain, a
        # failed turn.
        #
        # The two are told apart by STRUCTURE, not by keywords. A vendor
        # CLI reporting a spent window has never seen Crewlet's response
        # contract, so it cannot produce a valid envelope; the model
        # answering the turn does. So a parsed envelope settles it, and
        # the wording is only consulted where there was no contract to
        # violate — see :func:`_looks_like_a_usage_limit`.
        if not tool_calls and self._looks_like_a_usage_limit(text):
            logger.warning(
                "cli_llm_rate_limited",
                provider=self.provider_key,
                seat=workspace.seat,
            )
            raise ProviderCallError(
                f"{self.profile.binary} reported a usage limit: "
                f"{text.strip()[:_ERROR_EXCERPT_CHARS]}",
                ProviderErrorKind.RATE_LIMIT,
                provider_key=self.provider_key,
            )
        input_tokens = usage.get("input", 0) or estimate_tokens(prompt)
        output_tokens = usage.get("output", 0) or estimate_tokens(text)
        cache_read = usage.get("cache_read", 0)
        cache_write = usage.get("cache_write", 0)

        logger.info(
            "llm_complete",
            model=self.model,
            provider=self.provider_key,
            agent=self.profile.name,
            seat=workspace.seat,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            estimated_usage=not usage,
            tool_call_count=len(tool_calls),
        )
        return Completion(
            content=content,
            tool_calls=tool_calls,
            finish_reason="tool_calls" if tool_calls else "stop",
            tokens_used=input_tokens + output_tokens,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cache_read_input_tokens=cache_read,
            cache_creation_input_tokens=cache_write,
        )

    async def stream(
        self,
        messages: list[Message],
        tools: list[ToolDef] | None = None,
    ) -> AsyncIterator[CompletionChunk]:
        """Yield the completed answer as a single chunk.

        A headless CLI invocation is not incrementally consumable
        through this contract (its stdout is a result document, not a
        token stream), so streaming is satisfied by completing and
        emitting once. Crewlet's phases all use ``complete``; this
        exists so the provider is a full ``LLMProvider``.
        """
        completion = await self.complete(messages, tools)
        yield CompletionChunk(
            content=completion.content,
            tool_calls=completion.tool_calls,
            finish_reason=completion.finish_reason,
        )

    async def aclose(self) -> None:
        """Kill any still-running CLI child. Called by ``Engine.stop``."""
        self._closed = True
        for proc in list(self._children):
            await self._terminate(proc)
        self._children.clear()


def _loads_tolerant(text: str) -> Any:
    """Parse ``text`` as JSON, tolerating a banner before the object.

    Several CLIs print an update notice or a warning line ahead of their
    JSON document. Trying the whole buffer, then the last line, then the
    last balanced object recovers all three shapes without a mode flag.
    """
    import json as _json

    stripped = (text or "").strip()
    if not stripped:
        return None
    try:
        return _json.loads(stripped)
    except (ValueError, TypeError):
        pass
    lines = [line for line in stripped.splitlines() if line.strip()]
    if lines:
        try:
            return _json.loads(lines[-1])
        except (ValueError, TypeError):
            pass
    start = stripped.find("{")
    end = stripped.rfind("}")
    if start >= 0 and end > start:
        try:
            return _json.loads(stripped[start : end + 1])
        except (ValueError, TypeError):
            pass
    return None


__all__ = ["AuthMode", "CLIAgentConfigError", "CLIAgentProvider"]
