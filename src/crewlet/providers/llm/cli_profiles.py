"""Declarative profiles for subscription-authenticated coding CLIs.

A *CLI agent* is a locally installed command — ``claude``, ``codex``,
``gemini``, ``opencode``, … — that answers prompts using the operator's
**subscription** (Claude Pro/Max, ChatGPT Plus/Pro, Google AI Pro, a
Copilot seat) instead of a metered API key.  Crewlet drives one as an
:class:`~crewlet.providers.llm.protocol.LLMProvider` via
:mod:`crewlet.providers.llm.cli_agent`.

Every CLI-specific fact lives here, as **data**:

- how to invoke it non-interactively and where the prompt goes,
- how to read the answer (and token usage) out of its output,
- which environment variables relocate its state, which files under
  that state are *credentials* (shared across seats, synced on OAuth
  refresh) and which are *memory* (per seat, wiped after every call),
- how an operator logs in, logs out, and checks status.

Nothing here is hardcoded into the provider, so:

1. **A profile is fully overridable from YAML.**  Every field can be
   replaced under ``providers.llm.<key>.cli.overrides``, which is the
   answer to CLI flag drift — a vendor renaming ``--output-format`` is
   a config edit, not a Crewlet release.
2. **Adding a CLI needs no engine change.**  ``agent: custom`` plus an
   ``overrides`` block is a complete, first-class backend.

> **Flags drift.**  Each built-in profile records the CLI version it
> was written against in :attr:`CLIAgentProfile.verified_against`.
> ``crewlet llm doctor`` runs the binary and a real smoke completion so
> a mismatch surfaces at setup time rather than inside an agent's first
> turn.  See ``docs/concepts/subscription-llm-backends.md``.
"""

from __future__ import annotations

import json
from typing import Any, Literal, get_args

from pydantic import BaseModel, ConfigDict, Field

#: Every built-in profile name.  A ``Literal`` (not a free string) so
#: ``providers.llm.<key>.cli.agent`` rejects a typo at validation time
#: with the valid values listed, and so the generated JSON Schema drives
#: editor autocomplete.  Kept in lockstep with :data:`_BUILTIN` by an
#: import-time assertion and by ``tests/test_providers/test_cli_profiles``.
CLIAgentName = Literal[
    "claude-code",
    "codex",
    "gemini-cli",
    "qwen-code",
    "opencode",
    "cursor-agent",
    "copilot",
    "grok",
    "custom",
]

#: Placeholder substituted with the rendered prompt when
#: :attr:`CLIAgentProfile.prompt_via` is ``"argv"``.
PROMPT_PLACEHOLDER = "{prompt}"

#: Placeholder substituted with ``providers.llm.<key>.model``.
MODEL_PLACEHOLDER = "{model}"

#: Placeholder substituted with the seat's isolated home directory.
HOME_PLACEHOLDER = "{home}"

#: Default wording every vendor uses for "your subscription window is
#: spent".  Shared across profiles because the phrasing is remarkably
#: uniform, and overridable per profile for a CLI that says it
#: differently.  Matched case-insensitively against the CLI's combined
#: output.
DEFAULT_RATE_LIMIT_PATTERNS: tuple[str, ...] = (
    r"usage limit reached",
    r"rate[ _-]?limit",
    r"too many requests",
    r"quota (?:exceeded|exhausted)",
    r"\b429\b",
    r"you(?:'ve| have) (?:hit|reached) your .{0,40}limit",
    r"resets? (?:at|in) ",
    r"insufficient[ _-]?quota",
    r"overloaded",
)

#: Default wording for "this login is no longer valid".
DEFAULT_AUTH_ERROR_PATTERNS: tuple[str, ...] = (
    r"not (?:logged in|authenticated)",
    r"please (?:run )?(?:`)?[a-z-]+ login",
    r"authentication (?:failed|error|required)",
    r"invalid (?:api[ _-]?key|token|credentials?)",
    r"unauthorized",
    r"\b401\b",
    r"\b403\b",
    r"credentials? (?:expired|not found)",
    r"session (?:expired|has expired)",
    r"oauth token (?:expired|revoked)",
)


class StdinLoginSpec(BaseModel):
    """A non-interactive login that reads a credential from stdin.

    This is the ``optionally ask for user/pass`` path.  It is declared
    per profile because it is only *honest* where the CLI genuinely
    accepts a credential on stdin: the built-in vendor profiles for
    Claude / Codex / Gemini leave it unset, because those subscriptions
    authenticate through a browser OAuth (PKCE) flow with MFA and bot
    detection that no script can drive.  Faking it — typing a password
    into a headless browser — would be a workaround that breaks on the
    vendor's next login-page change, so Crewlet does not pretend.

    Where a credential login *does* exist (an operator's own wrapper, a
    self-hosted gateway CLI, ``opencode auth login``'s key prompt), set
    this and ``crewlet llm login --username … --password-stdin`` drives
    it.
    """

    model_config = ConfigDict(extra="forbid")

    args: list[str] = Field(default_factory=list)
    """Argv appended to the binary.  ``{username}`` is substituted."""

    stdin_template: str = "{password}\n"
    """What is written to the process's stdin.  ``{username}`` and
    ``{password}`` are substituted.  A CLI that prompts for both on one
    line needs ``"{username}\\n{password}\\n"``."""

    env: dict[str, str] = Field(default_factory=dict)
    """Extra env for the login process.  ``{username}`` / ``{password}``
    are substituted, so a CLI that reads ``FOO_PASSWORD`` from the
    environment instead of stdin is expressible here."""


class CLIAgentProfile(BaseModel):
    """Everything Crewlet needs to know about one coding CLI."""

    model_config = ConfigDict(extra="forbid")

    name: str
    """Profile id — the value of ``providers.llm.<key>.cli.agent``."""

    binary: str
    """Executable name resolved on ``PATH`` (or an absolute path)."""

    vendor: Literal["anthropic", "openai", "google", "xai", "other"] = "other"
    """Which model family this CLI actually talks to.

    Not cosmetic: the sandbox's OpenCode runner addresses a model as
    ``<family>/<model>``, and it can only derive that family from the
    provider. For an API entry the ``providers.llm`` *type* carries it,
    but every subscription entry has the same type (``cli-agent``) — so
    without this, a Claude subscription's ``sonnet`` would be addressed
    as ``openai/sonnet``. ``other`` means "the operator chooses" (the
    provider-agnostic CLIs), and falls back to the OpenAI family exactly
    as an unknown type always has."""

    verified_against: str = ""
    """Human-readable note naming the CLI release this profile's flags
    were written against.  Printed by ``crewlet llm doctor`` next to the
    binary's actual ``--version`` so drift is visible."""

    # ---------------------------------------------------------------
    # Invocation
    # ---------------------------------------------------------------

    version_args: list[str] = Field(default_factory=lambda: ["--version"])
    """Argv for the cheap liveness/version probe."""

    complete_args: list[str] = Field(default_factory=list)
    """Argv for one non-interactive completion.  May contain
    :data:`MODEL_PLACEHOLDER`, :data:`PROMPT_PLACEHOLDER` and
    :data:`HOME_PLACEHOLDER`."""

    prompt_via: Literal["stdin", "argv"] = "stdin"
    """Where the prompt goes.

    ``stdin`` is the default and the *right* default: a Crewlet prompt
    carries the whole phase transcript plus the tool-definition block
    and routinely runs to hundreds of kilobytes, which is a large
    fraction of the kernel's ``ARG_MAX`` (2 MiB on Linux, 256 KiB on
    macOS).  Passing it as an argument would work in testing and then
    fail with ``E2BIG`` on the first long turn — and would expose the
    prompt in ``ps`` output.  Use ``argv`` only for a CLI with no stdin
    mode, and expect a ceiling."""

    model_args: list[str] = Field(default_factory=list)
    """Appended only when the provider has a model configured."""

    output_format: Literal["json", "jsonl", "text"] = "json"
    """How to read stdout.  ``json``: one object.  ``jsonl``: a stream
    of events, one JSON object per line (non-JSON lines ignored).
    ``text``: stdout *is* the answer."""

    jsonl_text_mode: Literal["last", "concat"] = "last"
    """For ``jsonl``: whether the answer is the LAST matching text
    (event-per-message CLIs) or the CONCATENATION of every match
    (delta-streaming CLIs)."""

    # ---------------------------------------------------------------
    # Output extraction — declarative JSON paths, first match wins
    # ---------------------------------------------------------------

    text_paths: list[list[str]] = Field(default_factory=list)
    """Candidate paths to the assistant text, most specific first."""

    input_token_paths: list[list[str]] = Field(default_factory=list)
    output_token_paths: list[list[str]] = Field(default_factory=list)
    cache_read_token_paths: list[list[str]] = Field(default_factory=list)
    cache_write_token_paths: list[list[str]] = Field(default_factory=list)
    error_paths: list[list[str]] = Field(default_factory=list)
    """Paths that, when non-empty, mark the run as failed."""

    # ---------------------------------------------------------------
    # State isolation
    # ---------------------------------------------------------------

    home_env: list[str] = Field(default_factory=lambda: ["HOME"])
    """Env vars pointed at the seat's private home directory."""

    xdg: bool = True
    """Also set ``XDG_{CONFIG,DATA,CACHE,STATE}_HOME`` under the seat
    home.  Node CLIs increasingly honour XDG, and a CLI that ignores it
    is unaffected — so this defaults on."""

    dir_env: dict[str, str] = Field(default_factory=dict)
    """Vendor-specific relocation vars: env var → path **relative to the
    seat home**.  ``CLAUDE_CONFIG_DIR`` / ``CODEX_HOME`` live here."""

    credential_paths: list[str] = Field(default_factory=list)
    """Files under the seat home that hold auth material.  These are
    seeded from the provider's shared credential store before a run and
    synced back after it (OAuth access tokens refresh mid-run, and many
    providers rotate the refresh token with them — dropping the write
    would log every seat out at the next expiry)."""

    volatile_paths: list[str] = Field(default_factory=list)
    """Paths under the seat home holding conversation memory: sessions,
    transcripts, history, todo state, project notes.  Removed after
    **every** completion.  This is what makes a CLI backend safe to
    share across seats — nothing a seat said can survive into the next
    call, let alone another seat's."""

    seed_files: dict[str, str] = Field(default_factory=dict)
    """Files written into the seat home before every run: path relative
    to the home → literal content.  Used to pin a CLI's settings
    (telemetry off, auto-update off, no tool permissions) without
    depending on a flag."""

    env: dict[str, str] = Field(default_factory=dict)
    """Fixed environment for every invocation.  ``{home}`` is
    substituted."""

    passthrough_env: list[str] = Field(default_factory=list)
    """Extra host env var names this CLI needs beyond the shared
    allowlist in :mod:`crewlet.providers.llm.cli_workspace`."""

    # ---------------------------------------------------------------
    # Authentication
    # ---------------------------------------------------------------

    token_env: str = ""
    """Env var carrying a long-lived **subscription** token, when the
    CLI supports one (``CLAUDE_CODE_OAUTH_TOKEN``).  This is the best
    headless path there is: no credential files, nothing to sync, and
    it survives an ephemeral container."""

    api_key_env: str = ""
    """Env var for the metered API-key fallback (``auth.mode:
    api-key``)."""

    login_args: list[str] = Field(default_factory=list)
    """Argv for the interactive login.  Run by ``crewlet llm login``
    attached to the operator's terminal, inside the provider's shared
    credential home, so the vendor's own browser / device-code flow
    runs unmodified."""

    logout_args: list[str] = Field(default_factory=list)
    status_args: list[str] = Field(default_factory=list)

    token_capture_args: list[str] = Field(default_factory=list)
    """Argv for a command that *prints* a reusable token (``claude
    setup-token``).  ``crewlet llm login --capture-token`` runs it and
    stores the match in the encrypted secret store under
    :attr:`token_env`."""

    token_capture_pattern: str = ""
    """Regex whose first group is the token in ``token_capture_args``
    output.  Empty means "the last non-empty line"."""

    stdin_login: StdinLoginSpec | None = None
    """Credential (username / password) login, where the CLI has one.
    See :class:`StdinLoginSpec`."""

    # ---------------------------------------------------------------
    # Failure classification
    # ---------------------------------------------------------------

    rate_limit_patterns: list[str] = Field(
        default_factory=lambda: list(DEFAULT_RATE_LIMIT_PATTERNS)
    )
    """Case-insensitive regexes that mark a failed run as *rate
    limited* rather than broken.

    This is what makes a subscription backend composable: a role whose
    chain is ``[claude-cli, anthropic]`` runs on the flat-rate
    subscription until the five-hour window is spent, then falls
    through to the metered key for the rest of the window — because
    :class:`~crewlet.providers.fallback.FallbackLLMProvider` retries
    ``RATE_LIMIT``.  A CLI signals the limit as *prose on a successful
    exit*, so there is nothing but the text to match on."""

    auth_error_patterns: list[str] = Field(
        default_factory=lambda: list(DEFAULT_AUTH_ERROR_PATTERNS)
    )
    """Case-insensitive regexes that mark a failed run as an auth
    problem (expired login, revoked token) — retryable across the
    chain, and the signal ``crewlet llm doctor`` reports as "log in
    again"."""

    def merged(self, overrides: dict[str, Any] | None) -> CLIAgentProfile:
        """Return this profile with ``overrides`` applied field-wise.

        A shallow, whole-field replace: giving ``complete_args``
        replaces the list rather than appending to it, because a
        partial merge of an argv list has no sane semantics (position
        matters).  ``None`` values are dropped so a valueless YAML key
        means "leave alone", matching
        :class:`~crewlet.config.AuthoredModel`.
        """
        if not overrides:
            return self
        clean = {k: v for k, v in overrides.items() if v is not None}
        if not clean:
            return self
        return CLIAgentProfile.model_validate({**self.model_dump(), **clean})


# ---------------------------------------------------------------------
# Shared settings payloads
# ---------------------------------------------------------------------

# Claude Code reads this from ``CLAUDE_CONFIG_DIR``.  Two jobs:
#
# * ``permissions.deny`` — the CLI is being used as a *text* model, not
#   an agent.  Its own Bash/Edit/Write tools have no business running:
#   the seat home is throwaway, and a tool call is a wasted round at
#   best.  We do NOT pass a permissive ``--permission-mode``, so the
#   default already refuses; the deny list makes that explicit and
#   version-independent.
# * the telemetry / autoupdate flags — an engine-driven CLI must not
#   mutate its own binary mid-fleet, and each seat is a fresh install
#   as far as the updater is concerned.
_CLAUDE_SETTINGS = json.dumps(
    {
        "permissions": {
            "deny": [
                "Bash",
                "Edit",
                "MultiEdit",
                "NotebookEdit",
                "Read",
                "Task",
                "WebFetch",
                "WebSearch",
                "Write",
            ]
        },
        "includeCoAuthoredBy": False,
        "cleanupPeriodDays": 1,
    },
    indent=2,
)

_GEMINI_SETTINGS = json.dumps(
    {
        "telemetry": {"enabled": False},
        "usageStatisticsEnabled": False,
        "checkpointing": {"enabled": False},
    },
    indent=2,
)


# ---------------------------------------------------------------------
# Built-in profiles
# ---------------------------------------------------------------------

CLAUDE_CODE = CLIAgentProfile(
    name="claude-code",
    binary="claude",
    vendor="anthropic",
    verified_against="Claude Code CLI 2.x (`claude --version`)",
    complete_args=[
        "--print",
        "--output-format",
        "json",
        # No MCP servers: this backend is a text model, its tools come
        # from Crewlet's own registry via the response protocol.
        "--strict-mcp-config",
        "--mcp-config",
        '{"mcpServers":{}}',
    ],
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="json",
    text_paths=[["result"], ["content"]],
    input_token_paths=[["usage", "input_tokens"]],
    output_token_paths=[["usage", "output_tokens"]],
    cache_read_token_paths=[["usage", "cache_read_input_tokens"]],
    cache_write_token_paths=[["usage", "cache_creation_input_tokens"]],
    error_paths=[["error"]],
    dir_env={"CLAUDE_CONFIG_DIR": ".claude"},
    credential_paths=[".claude/.credentials.json"],
    volatile_paths=[
        ".claude/projects",
        ".claude/todos",
        ".claude/shell-snapshots",
        ".claude/statsig",
        ".claude/history.jsonl",
        ".claude.json",
    ],
    seed_files={".claude/settings.json": _CLAUDE_SETTINGS},
    env={"DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"},
    token_env="CLAUDE_CODE_OAUTH_TOKEN",
    api_key_env="ANTHROPIC_API_KEY",
    login_args=["/login"],
    logout_args=["/logout"],
    token_capture_args=["setup-token"],
    # `claude setup-token` prints a banner then the token on its own
    # line.  The token is an opaque OAuth artifact; anchor on its
    # documented prefix rather than "the last line", which a trailing
    # hint line would break.
    token_capture_pattern=r"(sk-ant-oat[0-9]{2}-[A-Za-z0-9_-]{20,})",
)

CODEX = CLIAgentProfile(
    name="codex",
    binary="codex",
    vendor="openai",
    verified_against="OpenAI Codex CLI 0.x (`codex --version`)",
    complete_args=[
        "exec",
        "--json",
        # The seat's work dir is a throwaway scratch directory, not a
        # checkout; without this Codex refuses to start.
        "--skip-git-repo-check",
        # Read-only: this backend answers prompts, it does not edit a
        # workspace.  Codex always carries its own tools; denying writes
        # is the version-stable way to keep them inert.
        "--sandbox",
        "read-only",
        "-",
    ],
    prompt_via="stdin",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="jsonl",
    jsonl_text_mode="last",
    text_paths=[
        ["item", "text"],
        ["msg", "message"],
        ["msg", "last_agent_message"],
        ["last_agent_message"],
    ],
    input_token_paths=[
        ["item", "usage", "input_tokens"],
        ["msg", "info", "total_token_usage", "input_tokens"],
        ["usage", "input_tokens"],
    ],
    output_token_paths=[
        ["item", "usage", "output_tokens"],
        ["msg", "info", "total_token_usage", "output_tokens"],
        ["usage", "output_tokens"],
    ],
    cache_read_token_paths=[
        ["msg", "info", "total_token_usage", "cached_input_tokens"],
        ["usage", "cached_input_tokens"],
    ],
    dir_env={"CODEX_HOME": ".codex"},
    credential_paths=[".codex/auth.json"],
    volatile_paths=[
        ".codex/sessions",
        ".codex/history.jsonl",
        ".codex/log",
        ".codex/archived_sessions",
    ],
    api_key_env="OPENAI_API_KEY",
    login_args=["login"],
    logout_args=["logout"],
    status_args=["login", "status"],
)

GEMINI_CLI = CLIAgentProfile(
    name="gemini-cli",
    binary="gemini",
    vendor="google",
    verified_against="Gemini CLI 0.x (`gemini --version`)",
    complete_args=["--output-format", "json"],
    prompt_via="stdin",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="json",
    text_paths=[["response"], ["result"]],
    input_token_paths=[["stats", "metrics", "tokens", "prompt"]],
    output_token_paths=[["stats", "metrics", "tokens", "candidates"]],
    cache_read_token_paths=[["stats", "metrics", "tokens", "cached"]],
    error_paths=[["error", "message"]],
    credential_paths=[".gemini/oauth_creds.json", ".gemini/google_accounts.json"],
    volatile_paths=[".gemini/tmp", ".gemini/history", ".gemini/sessions"],
    seed_files={".gemini/settings.json": _GEMINI_SETTINGS},
    api_key_env="GEMINI_API_KEY",
    passthrough_env=["GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION"],
    # Gemini CLI has no dedicated `login` verb: running it bare on a
    # fresh config dir starts the auth picker.
    login_args=[],
)

QWEN_CODE = CLIAgentProfile(
    name="qwen-code",
    binary="qwen",
    vendor="openai",
    verified_against="Qwen Code 0.x (`qwen --version`), a Gemini CLI fork",
    complete_args=["--output-format", "json"],
    prompt_via="stdin",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="json",
    text_paths=[["response"], ["result"]],
    input_token_paths=[["stats", "metrics", "tokens", "prompt"]],
    output_token_paths=[["stats", "metrics", "tokens", "candidates"]],
    credential_paths=[".qwen/oauth_creds.json"],
    volatile_paths=[".qwen/tmp", ".qwen/history", ".qwen/sessions"],
    api_key_env="OPENAI_API_KEY",
    login_args=[],
)

OPENCODE = CLIAgentProfile(
    name="opencode",
    binary="opencode",
    verified_against="OpenCode 0.x (`opencode --version`)",
    complete_args=["run", "--format", "json"],
    prompt_via="stdin",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="jsonl",
    # OpenCode streams `part.text` deltas rather than one final
    # message, so the answer is the concatenation (this matches the
    # sandbox runner in crewlet.sandbox.coding_agents.opencode).
    jsonl_text_mode="concat",
    text_paths=[["part", "text"]],
    input_token_paths=[["tokens", "input"]],
    output_token_paths=[["tokens", "output"]],
    cache_read_token_paths=[["tokens", "cache", "read"]],
    # OpenCode is XDG-native; `xdg=True` (the default) already points
    # ~/.local/share/opencode and ~/.config/opencode at the seat home.
    credential_paths=[".local/share/opencode/auth.json"],
    volatile_paths=[
        ".local/share/opencode/storage",
        ".local/share/opencode/log",
        ".local/state/opencode",
    ],
    api_key_env="ANTHROPIC_API_KEY",
    login_args=["auth", "login"],
    logout_args=["auth", "logout"],
    status_args=["auth", "list"],
    # `opencode auth login` walks a provider picker and then reads the
    # credential from stdin, so a scripted credential login is real
    # here — unlike the browser-OAuth vendors.
    stdin_login=StdinLoginSpec(args=["auth", "login"], stdin_template="{password}\n"),
)

CURSOR_AGENT = CLIAgentProfile(
    name="cursor-agent",
    binary="cursor-agent",
    verified_against="Cursor CLI 2026.x (`cursor-agent --version`)",
    complete_args=["--print", "--output-format", "json"],
    prompt_via="stdin",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="json",
    text_paths=[["result"], ["response"]],
    input_token_paths=[["usage", "input_tokens"]],
    output_token_paths=[["usage", "output_tokens"]],
    credential_paths=[".local/share/cursor-agent/credentials.json", ".cursor/cli.json"],
    volatile_paths=[
        ".local/share/cursor-agent/chats",
        ".local/share/cursor-agent/logs",
    ],
    login_args=["login"],
    logout_args=["logout"],
    status_args=["status"],
)

COPILOT_CLI = CLIAgentProfile(
    name="copilot",
    binary="copilot",
    verified_against="GitHub Copilot CLI 0.x (`copilot --version`)",
    complete_args=["--prompt", PROMPT_PLACEHOLDER, "--no-color"],
    # Copilot CLI takes the prompt as a flag value; long transcripts are
    # therefore bounded by ARG_MAX on this backend (see `prompt_via`).
    prompt_via="argv",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="text",
    dir_env={"COPILOT_CONFIG_DIR": ".copilot"},
    credential_paths=[".copilot/config.json"],
    volatile_paths=[".copilot/history-session-state", ".copilot/logs"],
    passthrough_env=["GITHUB_TOKEN", "GH_TOKEN"],
    login_args=["/login"],
    status_args=["/user", "show"],
)

GROK = CLIAgentProfile(
    name="grok",
    binary="grok",
    vendor="xai",
    verified_against="xAI Grok CLI 0.x (`grok --version`)",
    complete_args=["--output-format", "json"],
    prompt_via="stdin",
    model_args=["--model", MODEL_PLACEHOLDER],
    output_format="json",
    text_paths=[["response"], ["result"], ["text"]],
    input_token_paths=[["usage", "prompt_tokens"], ["usage", "input_tokens"]],
    output_token_paths=[["usage", "completion_tokens"], ["usage", "output_tokens"]],
    credential_paths=[".grok/auth.json", ".grok/user-settings.json"],
    volatile_paths=[".grok/sessions", ".grok/history"],
    api_key_env="GROK_API_KEY",
    passthrough_env=["XAI_API_KEY"],
    login_args=["login"],
    status_args=["status"],
)

CUSTOM = CLIAgentProfile(
    name="custom",
    binary="",
    verified_against="operator-defined",
    output_format="text",
)
"""Empty profile for a CLI Crewlet ships no knowledge of.

Every field is supplied under ``cli.overrides``; ``binary`` and
``complete_args`` are then required (the provider refuses to construct
without them).
"""


_BUILTIN: dict[str, CLIAgentProfile] = {
    p.name: p
    for p in (
        CLAUDE_CODE,
        CODEX,
        GEMINI_CLI,
        QWEN_CODE,
        OPENCODE,
        CURSOR_AGENT,
        COPILOT_CLI,
        GROK,
        CUSTOM,
    )
}

#: Names accepted by ``providers.llm.<key>.cli.agent``.
CLI_AGENT_NAMES: tuple[str, ...] = get_args(CLIAgentName)

# A profile that exists in one and not the other is a backend an
# operator can name but the engine cannot build (or vice versa) — fail
# at import rather than at that operator's first turn.
assert set(CLI_AGENT_NAMES) == set(_BUILTIN), (
    "CLIAgentName and the built-in profile registry disagree: "
    f"{sorted(set(CLI_AGENT_NAMES) ^ set(_BUILTIN))}"
)


def get_profile(name: str) -> CLIAgentProfile:
    """Return the built-in profile called ``name``.

    Raises ``KeyError`` with the valid names for an unknown value —
    reachable only from a hand-built config, since the YAML field is a
    ``Literal`` over :data:`CLI_AGENT_NAMES`.
    """
    try:
        return _BUILTIN[name]
    except KeyError:
        raise KeyError(
            f"unknown CLI agent profile {name!r}; "
            f"known profiles: {', '.join(CLI_AGENT_NAMES)}"
        ) from None


def resolve_profile(
    name: str, overrides: dict[str, Any] | None = None
) -> CLIAgentProfile:
    """Return the built-in profile ``name`` with ``overrides`` applied."""
    return get_profile(name).merged(overrides)


__all__ = [
    "CLI_AGENT_NAMES",
    "CLIAgentName",
    "DEFAULT_AUTH_ERROR_PATTERNS",
    "DEFAULT_RATE_LIMIT_PATTERNS",
    "HOME_PLACEHOLDER",
    "MODEL_PLACEHOLDER",
    "PROMPT_PLACEHOLDER",
    "CLIAgentProfile",
    "StdinLoginSpec",
    "get_profile",
    "resolve_profile",
]
