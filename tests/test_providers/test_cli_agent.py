"""``CLIAgentProvider`` end to end against a scripted fake CLI.

The fake is a real executable on ``PATH``, so these exercise the actual
subprocess path: argv construction, stdin delivery, the environment the
child sees, output decoding, failure classification, and cancellation —
everything except the vendor's own binary.
"""

from __future__ import annotations

import asyncio
import json
import os
import stat
import sys

import pytest

from crewlet.providers.errors import ProviderCallError, ProviderErrorKind, classify
from crewlet.providers.llm.cli_agent import CLIAgentConfigError, CLIAgentProvider
from crewlet.providers.llm.cli_profiles import CLIAgentProfile
from crewlet.providers.llm.protocol import Message, ToolDef
from crewlet.providers.llm.scope import ENGINE_SEAT, bind_llm_scope

TOOL = ToolDef(
    name="submit_plan",
    description="Submit the plan.",
    parameters={"type": "object", "properties": {"steps": {"type": "array"}}},
)


def write_script(tmp_path, name: str, body: str) -> str:
    """Drop an executable python script into a fresh bin dir on PATH."""
    bindir = tmp_path / "bin"
    bindir.mkdir(exist_ok=True)
    script = bindir / name
    script.write_text(f"#!{sys.executable}\n{body}")
    script.chmod(script.stat().st_mode | stat.S_IEXEC | stat.S_IRUSR)
    return str(bindir)


def make_provider(tmp_path, *, profile_kwargs=None, **kwargs) -> CLIAgentProvider:
    base = {
        "name": "fake",
        "binary": "fakecli",
        "complete_args": ["--json"],
        "output_format": "json",
        "text_paths": [["result"]],
        "input_token_paths": [["usage", "input_tokens"]],
        "output_token_paths": [["usage", "output_tokens"]],
        "credential_paths": [".fake/auth.json"],
        "volatile_paths": [".fake/sessions"],
    }
    base.update(profile_kwargs or {})
    params = {
        "provider_key": "default",
        "profile": CLIAgentProfile(**base),
        "model": "fast",
        "state_dir": str(tmp_path / "state"),
        "timeout": 20.0,
        "max_concurrent": 2,
    }
    params.update(kwargs)
    return CLIAgentProvider(**params)


ECHO_ENVELOPE = """
import json, sys
prompt = sys.stdin.read()
envelope = {"message": "planned", "tool_calls": [
    {"name": "submit_plan", "arguments": {"steps": ["a"]}}]}
print(json.dumps({
    "result": "```json\\n" + json.dumps(envelope) + "\\n```",
    "usage": {"input_tokens": 11, "output_tokens": 7},
    "_prompt_chars": len(prompt),
}))
"""


class TestConstruction:
    def test_missing_binary_fails_fast_with_guidance(self, tmp_path):
        with pytest.raises(CLIAgentConfigError) as excinfo:
            make_provider(tmp_path, profile_kwargs={"binary": "definitely-not-here"})
        assert "not there" in str(excinfo.value)
        assert "overrides.binary" in str(excinfo.value)

    def test_custom_profile_without_a_binary_is_rejected(self, tmp_path):
        with pytest.raises(CLIAgentConfigError) as excinfo:
            make_provider(tmp_path, profile_kwargs={"binary": ""})
        assert "has no binary" in str(excinfo.value)

    def test_api_key_mode_without_a_key_is_rejected(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", "pass"))
        with pytest.raises(CLIAgentConfigError) as excinfo:
            make_provider(
                tmp_path,
                profile_kwargs={"api_key_env": "FAKE_KEY"},
                auth_mode="api-key",
            )
        assert "no key is reachable" in str(excinfo.value)

    def test_advertises_its_call_budget(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", "pass"))
        provider = make_provider(tmp_path, timeout=333.0)
        assert provider.call_timeout_seconds == 333.0


class TestCompletion:
    async def test_tool_call_round_trip(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", ECHO_ENVELOPE))
        provider = make_provider(tmp_path)
        completion = await provider.complete(
            [Message(role="user", content="plan it")], [TOOL]
        )
        assert completion.content == "planned"
        assert [c.name for c in completion.tool_calls] == ["submit_plan"]
        assert completion.tool_calls[0].arguments == {"steps": ["a"]}
        assert completion.finish_reason == "tool_calls"
        assert completion.input_tokens == 11
        assert completion.output_tokens == 7
        assert completion.tokens_used == 18

    async def test_prompt_reaches_the_cli_on_stdin(self, tmp_path, monkeypatch):
        script = """
import json, sys
print(json.dumps({"result": sys.stdin.read()}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        completion = await provider.complete(
            [Message(role="system", content="MARKER-SYSTEM")], None
        )
        assert "MARKER-SYSTEM" in completion.content

    async def test_prompt_can_go_on_argv(self, tmp_path, monkeypatch):
        script = """
import json, sys
print(json.dumps({"result": sys.argv[-1]}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(
            tmp_path,
            profile_kwargs={
                "prompt_via": "argv",
                "complete_args": ["--json", "{prompt}"],
            },
        )
        completion = await provider.complete(
            [Message(role="user", content="ARGV-MARKER")], None
        )
        assert "ARGV-MARKER" in completion.content

    async def test_model_flag_is_passed(self, tmp_path, monkeypatch):
        script = """
import json, sys
print(json.dumps({"result": " ".join(sys.argv[1:])}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(
            tmp_path,
            model="opus",
            profile_kwargs={"model_args": ["--model", "{model}"]},
        )
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert "--model opus" in completion.content

    async def test_usage_is_estimated_when_the_cli_reports_none(
        self, tmp_path, monkeypatch
    ):
        script = """
import json
print(json.dumps({"result": "a" * 400}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(
            tmp_path,
            profile_kwargs={"input_token_paths": [], "output_token_paths": []},
        )
        completion = await provider.complete([Message(role="user", content="x")], None)
        # Budgets must keep moving even without vendor numbers.
        assert completion.output_tokens == 100
        assert completion.input_tokens > 0

    async def test_jsonl_last_event_wins(self, tmp_path, monkeypatch):
        script = """
print('{"item": {"text": "first"}}')
print('not json')
print('{"item": {"text": "second"}, "usage": {"input_tokens": 3}}')
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(
            tmp_path,
            profile_kwargs={
                "output_format": "jsonl",
                "text_paths": [["item", "text"]],
                "input_token_paths": [["usage", "input_tokens"]],
            },
        )
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content == "second"
        assert completion.input_tokens == 3

    async def test_jsonl_concat_mode_joins_deltas(self, tmp_path, monkeypatch):
        script = """
print('{"part": {"text": "he"}}')
print('{"part": {"text": "llo"}}')
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(
            tmp_path,
            profile_kwargs={
                "output_format": "jsonl",
                "jsonl_text_mode": "concat",
                "text_paths": [["part", "text"]],
            },
        )
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content == "hello"

    async def test_banner_before_the_json_is_tolerated(self, tmp_path, monkeypatch):
        script = """
import json
print("npm notice: a new version is available")
print(json.dumps({"result": "ok"}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content == "ok"

    async def test_text_output_format(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", "print('plain')"))
        provider = make_provider(tmp_path, profile_kwargs={"output_format": "text"})
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content == "plain"

    async def test_prose_reply_degrades_to_content(self, tmp_path, monkeypatch):
        script = """
import json
print(json.dumps({"result": "I would rather explain than call a tool."}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        completion = await provider.complete(
            [Message(role="user", content="x")], [TOOL]
        )
        assert completion.tool_calls == []
        assert "rather explain" in completion.content
        assert completion.finish_reason == "stop"


class TestFailureClassification:
    async def test_nonzero_exit_raises_a_classified_error(self, tmp_path, monkeypatch):
        script = """
import sys
sys.stderr.write("boom")
sys.exit(3)
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        with pytest.raises(ProviderCallError) as excinfo:
            await provider.complete([Message(role="user", content="x")], None)
        assert classify(excinfo.value) is ProviderErrorKind.FATAL
        assert "boom" in str(excinfo.value)

    async def test_expired_login_classifies_as_auth(self, tmp_path, monkeypatch):
        script = """
import sys
sys.stderr.write("Not logged in. Please run `fakecli login`.")
sys.exit(1)
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        with pytest.raises(ProviderCallError) as excinfo:
            await provider.complete([Message(role="user", content="x")], None)
        assert classify(excinfo.value) is ProviderErrorKind.AUTH

    async def test_spent_subscription_classifies_as_rate_limit(
        self, tmp_path, monkeypatch
    ):
        """The window-is-spent message arrives as prose on a SUCCESSFUL
        exit; treating it as an answer would feed an apology into the
        phase instead of falling through to the metered provider."""
        script = """
import json
print(json.dumps({"result": "Usage limit reached. Resets at 4pm."}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        with pytest.raises(ProviderCallError) as excinfo:
            await provider.complete([Message(role="user", content="x")], None)
        assert classify(excinfo.value) is ProviderErrorKind.RATE_LIMIT

    async def test_an_answer_about_rate_limiting_is_not_a_rate_limit(
        self, tmp_path, monkeypatch
    ):
        """The wording these CLIs use is ordinary engineering prose.

        "rate limit", "429", "overloaded", "too many requests", "resets
        at" — an engineering agent company discusses all of them. Matched
        against the model's ANSWER, a plan to add rate limiting to a
        gateway was discarded as a provider outage: RATE_LIMIT is
        retryable, so a chain fell through to the metered key and a
        subscription-only chain failed the turn outright.
        """
        script = """
import json, sys
sys.stdin.read()
envelope = {"message": "here is the plan", "tool_calls": [
    {"name": "submit_plan",
     "arguments": {"steps": ["Add rate limiting to the gateway (429 + backoff)"]}}]}
print(json.dumps({"result": "```json\\n" + json.dumps(envelope) + "\\n```"}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)

        completion = await provider.complete(
            [Message(role="user", content="plan it")], [TOOL]
        )

        assert [c.name for c in completion.tool_calls] == ["submit_plan"]
        assert completion.tool_calls[0].arguments["steps"] == [
            "Add rate limiting to the gateway (429 + backoff)"
        ]

    async def test_a_long_answer_mentioning_limits_is_not_a_rate_limit(
        self, tmp_path, monkeypatch
    ):
        """The no-tool-surface path, where no envelope can settle it.

        A vendor's limit notice is a sentence; a Review summary or a
        chat answer explaining the team's rate-limit strategy is not.
        """
        answer = (
            "We hit the gateway's rate limit during the load test. "
            "The 429s came from the shared bucket, not from the client, "
            "so the fix is a per-tenant quota rather than more retries. "
        ) * 3
        script = f"""
import json
print(json.dumps({{"result": {answer!r}}}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)

        completion = await provider.complete([Message(role="user", content="x")], None)

        assert completion.content.startswith("We hit the gateway's rate limit")

    async def test_timeout_kills_the_process_and_classifies(
        self, tmp_path, monkeypatch
    ):
        script = """
import time
time.sleep(30)
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path, timeout=0.5)
        with pytest.raises(ProviderCallError) as excinfo:
            await provider.complete([Message(role="user", content="x")], None)
        assert classify(excinfo.value) is ProviderErrorKind.TIMEOUT

    async def test_rate_limit_falls_through_the_provider_chain(
        self, tmp_path, monkeypatch
    ):
        """The composition this feature exists for: subscription first,
        metered key when the window is spent."""
        from crewlet.providers.fallback import FallbackLLMProvider
        from crewlet.providers.llm.protocol import Completion

        script = """
import json
print(json.dumps({"result": "usage limit reached"}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        cli = make_provider(tmp_path)

        class Metered:
            model = "gpt-fallback"

            async def complete(self, messages, **kwargs):
                return Completion(content="answered by the API key")

        wrapper = FallbackLLMProvider([("subscription", cli), ("metered", Metered())])
        completion = await wrapper.complete([Message(role="user", content="x")])
        assert completion.content == "answered by the API key"
        assert wrapper.attempted_keys == ["subscription", "metered"]


class TestIsolationInFlight:
    async def test_child_runs_in_the_seats_home(self, tmp_path, monkeypatch):
        script = """
import json, os
print(json.dumps({"result": os.environ["HOME"]}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        with bind_llm_scope("sarah-chen"):
            first = await provider.complete([Message(role="user", content="x")], None)
        with bind_llm_scope("marcus-rivera"):
            second = await provider.complete([Message(role="user", content="x")], None)
        assert first.content != second.content
        assert first.content.endswith("seats/sarah-chen/home")
        assert second.content.endswith("seats/marcus-rivera/home")

    async def test_calls_outside_a_turn_use_the_engine_seat(
        self, tmp_path, monkeypatch
    ):
        script = """
import json, os
print(json.dumps({"result": os.environ["HOME"]}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(tmp_path)
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content.endswith(f"seats/{ENGINE_SEAT}/home")

    async def test_host_api_key_never_reaches_the_child(self, tmp_path, monkeypatch):
        script = """
import json, os
print(json.dumps({"result": os.environ.get("ANTHROPIC_API_KEY", "ABSENT")}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-metered")
        provider = make_provider(
            tmp_path, profile_kwargs={"api_key_env": "ANTHROPIC_API_KEY"}
        )
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content == "ABSENT"

    async def test_subscription_token_is_passed(self, tmp_path, monkeypatch):
        script = """
import json, os
print(json.dumps({"result": os.environ.get("FAKE_OAUTH", "ABSENT")}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        provider = make_provider(
            tmp_path,
            profile_kwargs={"token_env": "FAKE_OAUTH"},
            subscription_token="oat-123",
        )
        completion = await provider.complete([Message(role="user", content="x")], None)
        assert completion.content == "oat-123"

    async def test_concurrency_cap_is_enforced(self, tmp_path, monkeypatch):
        # Each invocation APPENDS one byte on entry and one on exit. A
        # single small append is atomic on POSIX, so concurrent writers
        # cannot corrupt the log — unlike a read-modify-write counter,
        # which is exactly the race this test is trying to observe and
        # would therefore lose to intermittently.
        script = """
import json, os, time
log = os.environ["FAKE_LOG"]
with open(log, "a") as f:
    f.write("+")
time.sleep(0.2)
with open(log, "a") as f:
    f.write("-")
print(json.dumps({"result": "ok"}))
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        log = tmp_path / "concurrency.log"
        log.write_text("")
        provider = make_provider(
            tmp_path,
            max_concurrent=2,
            profile_kwargs={"passthrough_env": ["FAKE_LOG"], "env": {}},
        )
        monkeypatch.setenv("FAKE_LOG", str(log))
        await asyncio.gather(
            *(
                provider.complete([Message(role="user", content="x")], None)
                for _ in range(6)
            )
        )
        events = log.read_text()
        assert events.count("+") == 6, events
        live = 0
        peak = 0
        for event in events:
            live += 1 if event == "+" else -1
            peak = max(peak, live)
        assert peak <= 2, f"{peak} processes ran at once: {events}"


class TestLifecycle:
    async def test_stream_yields_one_chunk(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", ECHO_ENVELOPE))
        provider = make_provider(tmp_path)
        chunks = [
            chunk
            async for chunk in provider.stream(
                [Message(role="user", content="x")], [TOOL]
            )
        ]
        assert len(chunks) == 1
        assert chunks[0].content == "planned"
        assert [c.name for c in chunks[0].tool_calls] == ["submit_plan"]

    async def test_aclose_refuses_further_calls(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", ECHO_ENVELOPE))
        provider = make_provider(tmp_path)
        await provider.aclose()
        with pytest.raises(ProviderCallError):
            await provider.complete([Message(role="user", content="x")], None)

    @pytest.mark.skipif(sys.platform == "win32", reason="POSIX process groups")
    async def test_cancellation_kills_the_child(self, tmp_path, monkeypatch):
        script = """
import os, pathlib, time
pathlib.Path(os.environ["FAKE_PIDFILE"]).write_text(str(os.getpid()))
time.sleep(30)
"""
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", script))
        pidfile = tmp_path / "pid"
        monkeypatch.setenv("FAKE_PIDFILE", str(pidfile))
        provider = make_provider(
            tmp_path, profile_kwargs={"passthrough_env": ["FAKE_PIDFILE"]}
        )
        task = asyncio.create_task(
            provider.complete([Message(role="user", content="x")], None)
        )
        for _ in range(100):
            if pidfile.exists():
                break
            await asyncio.sleep(0.05)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        pid = int(pidfile.read_text())
        with pytest.raises(OSError):
            for _ in range(40):
                os.kill(pid, 0)
                await asyncio.sleep(0.05)


class TestDecodingHelpers:
    def test_error_path_surfaces_the_message(self, tmp_path, monkeypatch):
        monkeypatch.setenv("PATH", write_script(tmp_path, "fakecli", "pass"))
        provider = make_provider(
            tmp_path, profile_kwargs={"error_paths": [["error", "message"]]}
        )
        text, _usage = provider._decode(
            json.dumps({"error": {"message": "model unavailable"}})
        )
        assert text == "model unavailable"
