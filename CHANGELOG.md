# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
The minor number moves for features, the patch number for fixes.

## [0.1.0] — Unreleased

Initial open-source release.

- Hierarchical agent-company engine: org model (units, roles, human seats),
  event-driven agent runtime with the Plan → Execute → Review turn engine,
  task engine, scheduler, DACI decision conventions
- Two-tier configuration: ops-owned bootstrap YAML + versioned, live-editable
  company config in PostgreSQL, with optional whole-config encryption at rest
- Encrypted secret store: a `secret_values` table the engine consults ahead of
  `os.environ` when resolving `${VAR}`, managed with `crewlet secrets
  set/list/unset/get/rekey` and writable directly by the provisioning CLIs
  (`--secret-store`), so a minted credential needs no env file to be sourced
- Providers: Anthropic, OpenAI, and any OpenAI-compatible LLM endpoint;
  OpenAI-compatible embeddings; Apache Pulsar event queue; PostgreSQL
  (TimescaleDB + pgvector)
- Integrations: Slack, Jira, Confluence, GitHub, GitLab, Plane (self-hosted
  fork) — with per-agent identities and provisioning CLIs for Slack (one app
  per agent via the App Manifest APIs), GitLab, and Plane.
  There is no email channel: agents' `email` is an *identity* used for routing
  and commit attribution, not a delivery transport
- Code sandbox: per-role sandboxed coding-agent execution (E2B cloud or
  self-hosted; Claude Code and OpenCode runners)
- Agent learning: private diary, episodic memory, skill synthesis/refinement/
  promotion, counterparty profiles, tool skills
- REST API + zero-build web dashboard, webhook receivers, structured logging,
  OpenTelemetry tracing, TimescaleDB event store
- Config authoring: `crewlet schema` emits JSON Schema for both tiers
  (checked into `schema/`) for editor autocomplete and CI; `crewlet validate`
  gains `--json` with per-field error paths and `--tier` so the Tier A file is
  checkable too; a `company-architect` skill (`skills/`) lets an AI assistant
  interview a founder and author the config, with a step-by-step
  walkthrough. See `docs/getting-started/ai-authoring.md`
- The authored org chart is now typed: `roles`, `units`, and `mcp_servers`
  validate through `RoleConfig` / `OrgUnitConfig` / `MCPServerConfig` instead
  of being untyped dicts. An unknown key in a role, unit, or MCP server entry
  is a validation error rather than being silently dropped — including a
  role-level `slack:` block, which the parser never read (use
  `integrations.slack`)
- `providers.llm[*].type` and `providers.embeddings.type` are closed sets
  (`openai` / `anthropic` / `openai-compatible`). A typo used to pass
  validation and then fall through every branch of the provider factory,
  leaving that provider key silently absent with no error — roles naming it
  ran without an LLM. The factory raises instead of falling through
- The generated schema carries the cross-field rules it can express (knowledge
  backend exclusivity, human seats needing a contact, handle and cron shape),
  so an editor or an AI assistant catches them **without crewlet installed**.
  Reference integrity (`lead` / `manages`), IANA timezones, and cron semantics
  remain loader-only and are documented as such
- Published to PyPI as [`crewlet`](https://pypi.org/project/crewlet/). Releases
  are cut by pushing a `v*` tag and published over OpenID Connect (PyPI Trusted
  Publishing) with signed PEP 740 attestations — no API token exists in the
  repository. CI builds and installs the distributions on every pull request.
  See `RELEASING.md`.
  Packaging fixes that came with it: the version is single-sourced from
  `crewlet.__version__` (it was duplicated in `pyproject.toml` and could
  drift); the deprecated `License :: OSI Approved :: MIT License` classifier is
  gone, since PEP 639 requires PyPI to reject it alongside the SPDX
  `License-Expression: MIT` the build already emitted; the `Documentation` URL
  pointed at a domain that does not exist (`crewlet.dev` → `docs.crewlet.ai`);
  README's repo-relative links and images are absolutised for the PyPI long
  description, where all 45 of them rendered dead; and the sdist no longer
  ships the agent-harness config (`CLAUDE.md`, `.claude/`)
- No `observability` extra: it declared no dependencies — the OpenTelemetry
  SDK and OTLP exporter are core requirements — so
  `pip install crewlet[observability]` only ever installed `crewlet`
- Python 3.13 is now tested in CI and declared supported alongside 3.12. Both
  the test suite (editable, `[dev,all]`, from the source tree) and a clean
  install of the built wheel (core dependencies only, the way PyPI serves it)
  run on every supported interpreter
- Dependency updates are automated. Dependabot watches all three surfaces the
  repository has — the workflow actions, `pyproject.toml`, and the dev stack's
  `docker-compose.yml` — on a weekly schedule
- The PyPI publish action is pinned to a release tag instead of the action's
  `release/v1` branch. Dependabot offers no updates for a ref it cannot read a
  version out of, which had left the one action handed a PyPI OIDC token as
  the only dependency in the repository nothing was watching
