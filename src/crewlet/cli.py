"""Crewlet CLI — run, validate, and serve agent organizations."""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import json
import logging
import sys
from pathlib import Path
from typing import Any

import yaml

from crewlet import __version__
from crewlet._logging import configure_logging, get_logger
from crewlet.config import CompanyConfig

logger = get_logger("cli")


def _print_safe(message: str) -> None:
    """Print *message* to stderr, tolerating a dead stream.

    Shutdown-time messages run after Ctrl+C, which the terminal
    delivers to the whole foreground process group — so a pipeline
    reader (``crewlet run 2>&1 | tee out``) is usually already dead
    and stderr is a broken pipe.  Raising here would replace the clean
    exit path with a ``BrokenPipeError`` traceback.
    """
    with contextlib.suppress(Exception):
        print(message, file=sys.stderr, flush=True)


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="crewlet",
        description="Engine for orchestrating AI agent companies.",
    )
    parser.add_argument(
        "-V", "--version", action="version", version=f"crewlet {__version__}"
    )
    sub = parser.add_subparsers(dest="command")

    # --- run ---
    run_p = sub.add_parser("run", help="Run an organization from a YAML config")
    run_p.add_argument(
        "config",
        type=Path,
        nargs="?",
        default=Path("config.yaml"),
        help=(
            "Path to the YAML config file (default: ./config.yaml). "
            "Pass an explicit path for non-standard locations."
        ),
    )
    run_p.add_argument("--debug", action="store_true", help="Enable debug logging")
    run_p.add_argument(
        "--api-port",
        type=int,
        default=0,
        help="Start embedded API server on this port (for webhooks)",
    )
    run_p.add_argument(
        "--import-company",
        type=Path,
        default=None,
        metavar="PATH",
        help=(
            "Before starting the engine, import the given Tier B company "
            "YAML into the company_config table if no active revision "
            "exists.  Idempotent: if a revision is already active the "
            "import is skipped (use `crewlet config import --force` to "
            "overwrite).  Enables a one-command bootstrap: "
            "`crewlet run config.yaml --import-company company.yaml`."
        ),
    )
    # One knowledge backend per run: config validation enforces
    # confluence × plane exclusivity, and the bundled imports mirror it
    # at the argparse layer so the error fires BEFORE either import runs.
    run_imports = run_p.add_mutually_exclusive_group()
    run_imports.add_argument(
        "--import-confluence",
        type=Path,
        nargs="?",
        const=Path("examples"),
        default=None,
        metavar="PATH",
        help=(
            "Before starting the engine, publish local .md files to "
            "Confluence — both tool-skill pages (frontmatter 'trigger') "
            "and knowledge docs (everything else; space = parent directory, "
            "title = first '# H1'). Optional path; defaults to examples/ "
            "when the flag is given without a value. Mutually exclusive "
            "with --import-plane."
        ),
    )
    run_p.add_argument(
        "--update-confluence",
        action="store_true",
        help=(
            "With --import-confluence: overwrite existing pages instead of "
            "skipping them."
        ),
    )
    run_p.add_argument(
        "--create-confluence-space",
        action="store_true",
        help=(
            "With --import-confluence: auto-create any target Confluence "
            "space that doesn't exist. Requires the bot account to have "
            "Confluence space-admin permission on the tenant."
        ),
    )
    run_p.add_argument(
        "--prune-confluence",
        action="store_true",
        help=(
            "With --import-confluence: also delete import-managed skill "
            "pages whose local source .md was removed (orphans). Only "
            "touches pages the importer published, never user-authored ones."
        ),
    )
    run_imports.add_argument(
        "--import-plane",
        type=Path,
        nargs="?",
        const=Path("examples"),
        default=None,
        metavar="PATH",
        help=(
            "Before starting the engine, publish local .md files to "
            "Plane — both tool-skill pages (frontmatter 'trigger') and "
            "knowledge docs (everything else; project = parent directory, "
            "title = first '# H1'). Optional path; defaults to examples/ "
            "when the flag is given without a value. Requires "
            "--import-company. Mutually exclusive with --import-confluence."
        ),
    )
    run_p.add_argument(
        "--update-plane",
        action="store_true",
        help=(
            "With --import-plane: overwrite existing pages instead of skipping them."
        ),
    )
    run_p.add_argument(
        "--prune-plane",
        action="store_true",
        help=(
            "With --import-plane: also archive+delete import-managed skill "
            "pages whose local source .md was removed (orphans). Only "
            "touches pages the importer published "
            "(external_source=crewlet), never user-authored ones or "
            "knowledge docs."
        ),
    )

    # --- validate ---
    val_p = sub.add_parser("validate", help="Validate a YAML config file")
    val_p.add_argument("config", type=Path, help="Path to the YAML config file")
    val_p.add_argument(
        "--tier",
        choices=["auto", "company", "bootstrap"],
        default="auto",
        help=(
            "Which tier the file is. 'auto' (default) reads a top-level "
            "'name' key as Tier B (company.yaml) and anything else as "
            "Tier A (config.yaml)"
        ),
    )
    val_p.add_argument(
        "--json",
        dest="as_json",
        action="store_true",
        help=(
            "Emit a machine-readable result on stdout: "
            '{"valid": bool, "tier": str, "errors": [{"path", "message", '
            '"type"}], "summary": {...}}. For editors and AI-assisted '
            "authoring loops (see docs/getting-started/ai-authoring.md)"
        ),
    )

    # --- migrate ---
    migrate_p = sub.add_parser(
        "migrate",
        help="Apply pending database migrations",
    )
    migrate_p.add_argument(
        "config",
        type=Path,
        nargs="?",
        default=Path("config.yaml"),
        help="Path to the Tier A YAML config file (default: ./config.yaml)",
    )
    migrate_p.add_argument(
        "--company",
        type=Path,
        default=None,
        help=(
            "Tier B company YAML to read the embedding width from, for a "
            "database with no active revision yet. The pgvector columns "
            "are fixed at creation and the sequence is forward-only, so "
            "without a width the run stops before them (and says so) "
            "rather than guessing"
        ),
    )
    migrate_p.add_argument(
        "--check",
        action="store_true",
        help="Report pending migrations and exit non-zero if any; apply nothing",
    )
    migrate_p.add_argument("--debug", action="store_true", help="Verbose logging")

    # --- budgets ---
    budgets_p = sub.add_parser(
        "budgets",
        help="Inspect or reset token-budget usage",
    )
    budgets_sub = budgets_p.add_subparsers(dest="budgets_command")

    bud_show = budgets_sub.add_parser("show", help="Show token usage per scope")
    bud_show.add_argument(
        "config",
        type=Path,
        nargs="?",
        default=Path("config.yaml"),
        help="Path to the Tier A YAML config file (default: ./config.yaml)",
    )
    bud_show.add_argument("--debug", action="store_true", help="Verbose logging")

    bud_reset = budgets_sub.add_parser(
        "reset",
        help="Zero token usage (usage is durable across restarts)",
    )
    bud_reset.add_argument(
        "config",
        type=Path,
        nargs="?",
        default=Path("config.yaml"),
        help="Path to the Tier A YAML config file (default: ./config.yaml)",
    )
    bud_reset.add_argument(
        "--scope",
        default="",
        help="Reset one scope ('org' or 'agent:<id>'); omit to reset every scope",
    )
    bud_reset.add_argument("--debug", action="store_true", help="Verbose logging")

    # --- schema ---
    schema_p = sub.add_parser(
        "schema",
        help="Print the JSON Schema for a config tier",
    )
    schema_p.add_argument(
        "tier",
        nargs="?",
        choices=["company", "bootstrap"],
        default="company",
        help="Which tier to describe (default: company)",
    )
    schema_p.add_argument(
        "-o",
        "--output",
        type=Path,
        default=None,
        help="Write to this path instead of stdout",
    )

    # --- confluence ---
    confluence_p = sub.add_parser(
        "confluence",
        help=(
            "Publish local pages to Confluence (skills + knowledge docs) / "
            "resync the tool-skill registry"
        ),
    )
    confluence_sub = confluence_p.add_subparsers(dest="confluence_command")
    imp_p = confluence_sub.add_parser(
        "import",
        help=(
            "Publish local .md files to Confluence — tool-skill pages "
            "(frontmatter 'trigger') and knowledge docs (everything else; "
            "space = parent directory, title = first '# H1')"
        ),
    )
    imp_p.add_argument("config", type=Path, help="Path to the Tier B company YAML")
    imp_p.add_argument(
        "path",
        type=Path,
        nargs="?",
        default=Path("examples"),
        help=(
            "File or directory of .md files to publish, walked recursively "
            "(default: examples/)"
        ),
    )
    imp_p.add_argument(
        "--space",
        type=str,
        default=None,
        help=(
            "Tool Skills space key for skill files "
            "(default: $CREWLET_TOOL_SKILLS_SPACE or 'TS'). Knowledge docs "
            "take their space from their parent directory."
        ),
    )
    imp_p.add_argument(
        "--update",
        action="store_true",
        help="Overwrite existing pages instead of skipping them",
    )
    imp_p.add_argument(
        "--dry-run",
        action="store_true",
        help="Show what would be created/updated without making API calls",
    )
    imp_p.add_argument(
        "--create-space",
        action="store_true",
        help=(
            "Auto-create any target Confluence space that doesn't exist. "
            "Requires the bot account to have Confluence space-admin "
            "permission on the tenant."
        ),
    )
    imp_p.add_argument(
        "--prune",
        action="store_true",
        help=(
            "After publishing, delete import-managed skill pages in the "
            "skill space whose source .md is gone (e.g. a removed bundled "
            "skill). Only touches pages the importer published; never "
            "user-authored pages or knowledge docs. Combine with --dry-run "
            "to preview deletions."
        ),
    )
    resync_p = confluence_sub.add_parser(
        "resync",
        help="Re-fetch the Tool Skills space and rebuild the in-memory registry",
    )
    resync_p.add_argument("config", type=Path, help="Path to the Tier B company YAML")
    resync_p.add_argument(
        "--space",
        type=str,
        default=None,
        help="Override the configured Tool Skills space key",
    )

    # --- gitlab ---
    from crewlet.gitlab.provision_cli import add_gitlab_parser

    add_gitlab_parser(sub)

    # --- mattermost ---
    from crewlet.mattermost.provision_cli import add_mattermost_parser

    add_mattermost_parser(sub)

    # --- plane ---
    plane_p = sub.add_parser(
        "plane",
        help=(
            "Publish local pages to Plane (skills + knowledge docs), "
            "resync the tool-skill registry, or provision service "
            "accounts, tokens, and the workspace webhook"
        ),
    )
    plane_sub = plane_p.add_subparsers(dest="plane_command")
    plane_imp = plane_sub.add_parser(
        "import",
        help=(
            "Publish local .md files to Plane — tool-skill pages "
            "(frontmatter 'trigger') and knowledge docs (everything else; "
            "project = parent directory, title = first '# H1')"
        ),
    )
    plane_imp.add_argument("config", type=Path, help="Path to the Tier B company YAML")
    plane_imp.add_argument(
        "path",
        type=Path,
        nargs="?",
        default=Path("examples"),
        help=(
            "File or directory of .md files to publish, walked recursively "
            "(default: examples/)"
        ),
    )
    plane_imp.add_argument(
        "--project",
        type=str,
        default=None,
        help=(
            "Tool Skills project identifier for skill files "
            "(default: $CREWLET_TOOL_SKILLS_PROJECT or 'TS'). Knowledge "
            "docs take their project from their parent directory."
        ),
    )
    plane_imp.add_argument(
        "--update",
        action="store_true",
        help="Overwrite existing pages instead of skipping them",
    )
    plane_imp.add_argument(
        "--dry-run",
        action="store_true",
        help="Show what would be created/updated without making page writes",
    )
    plane_imp.add_argument(
        "--prune",
        action="store_true",
        help=(
            "After publishing, archive+delete import-managed skill pages "
            "in the skills project whose source .md is gone (e.g. a removed "
            "bundled skill). Only touches pages the importer published "
            "(external_source=crewlet); never user-authored pages or "
            "knowledge docs. Combine with --dry-run to preview deletions."
        ),
    )
    plane_resync_p = plane_sub.add_parser(
        "resync",
        help="Re-fetch the Tool Skills project and rebuild the in-memory registry",
    )
    plane_resync_p.add_argument(
        "config", type=Path, help="Path to the Tier B company YAML"
    )
    plane_resync_p.add_argument(
        "--project",
        type=str,
        default=None,
        help="Override the configured Tool Skills project identifier",
    )

    from crewlet.plane.provision_cli import add_provision_parser

    add_provision_parser(plane_sub)

    # --- slack ---
    slack_p = sub.add_parser(
        "slack",
        help=(
            "Provision per-agent Slack apps from the company YAML via "
            "Slack's App Manifest APIs"
        ),
    )
    slack_sub = slack_p.add_subparsers(dest="slack_command")
    slack_prov = slack_sub.add_parser(
        "provision",
        help=(
            "Create/update one Slack app per Slack-enabled agent seat, run "
            "the OAuth install, and write the tokens into .env"
        ),
    )
    slack_prov.add_argument(
        "company_config", type=Path, help="Path to the Tier B company YAML"
    )
    slack_prov.add_argument(
        "--base-url",
        type=str,
        required=True,
        help=(
            "Public HTTPS base URL of the Crewlet API server — becomes each "
            "app's Events API request URL (…/webhooks/slack/{handle}) and "
            "the OAuth redirect URL (…/webhooks/slack-oauth)"
        ),
    )
    slack_prov.add_argument(
        "--secret-store",
        action="store_true",
        help=(
            "Write the obtained secrets into the encrypted secret_values "
            "table instead of an env file. The engine resolves ${VAR} from "
            "there directly, so nothing has to be sourced into a shell. "
            "Requires a Tier A keyring and DB DSN (--bootstrap / --dsn)."
        ),
    )
    slack_prov.add_argument(
        "--env-file",
        type=Path,
        default=None,
        help=(
            "The .env file secrets are read from AND written to "
            "(default: the .env `crewlet run` loads — beside the company "
            "YAML if one is there, else ./.env). Values in this file win "
            "over stale shell exports for the keys provisioning manages. "
            "Ignored with --secret-store."
        ),
    )
    slack_prov.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help=(
            "Path to the Tier A bootstrap YAML (supplies the DB DSN + "
            "encryption keyring for --secret-store)"
        ),
    )
    slack_prov.add_argument(
        "--dsn",
        type=str,
        default=None,
        help="Override DB DSN instead of reading from --bootstrap",
    )
    slack_prov.add_argument(
        "--state-file",
        type=Path,
        default=None,
        help=(
            "Provisioning ledger mapping agent handles to Slack app ids + "
            "app credentials (default: slack-apps.json next to the company "
            "YAML). Keep it out of version control, like .env."
        ),
    )
    slack_prov.add_argument(
        "--handles",
        type=str,
        default=None,
        help="Comma-separated agent handles to provision (default: all)",
    )
    slack_prov.add_argument(
        "--dry-run",
        action="store_true",
        help=(
            "Validate each manifest via apps.manifest.validate and print "
            "the plan; no app is created or updated. (May refresh the "
            "stored app-configuration token pair in the env file if it is "
            "missing or near expiry — validation needs a live token.)"
        ),
    )
    slack_prov.add_argument(
        "--skip-install",
        action="store_true",
        help=(
            "Create/update the apps and write signing secrets, but skip the "
            "interactive OAuth install (no bot tokens written) — for "
            "non-interactive runs, e.g. pushing a scope change to every app"
        ),
    )
    slack_prov.add_argument(
        "--reinstall",
        action="store_true",
        help=(
            "Run the OAuth install even for agents whose bot-token env var "
            "is already set (mints a fresh xoxb token, e.g. after a revoke)"
        ),
    )

    # --- config ---
    config_p = sub.add_parser(
        "config",
        help=("Manage the Tier B company configuration (versioned in PostgreSQL)"),
    )
    config_sub = config_p.add_subparsers(dest="config_command")

    # config import
    cfg_imp = config_sub.add_parser(
        "import",
        help="Load a Tier B YAML and write it as the active revision",
    )
    cfg_imp.add_argument(
        "company_config",
        type=Path,
        help="Path to the Tier B company YAML",
    )
    cfg_imp.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help=(
            "Path to the Tier A bootstrap YAML "
            "(default: ./config.yaml; supplies the DB DSN)"
        ),
    )
    cfg_imp.add_argument(
        "--dsn",
        type=str,
        default=None,
        help=(
            "Override DB DSN instead of reading from --bootstrap. "
            "Useful for ops scripts that don't carry a YAML."
        ),
    )
    cfg_imp.add_argument(
        "--force",
        action="store_true",
        help=(
            "Activate even when an active revision already exists "
            "(creates a new revision and demotes the prior)."
        ),
    )
    cfg_imp.add_argument(
        "--dry-run",
        action="store_true",
        help="Validate the YAML and print what would change; no DB write.",
    )
    cfg_imp.add_argument(
        "--summary",
        type=str,
        default=None,
        help="Audit summary recorded on the new revision (default: 'cli import')",
    )

    # config export
    cfg_exp = config_sub.add_parser(
        "export",
        help="Dump the active revision (or a specific one) as YAML to stdout",
    )
    cfg_exp.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help="Path to the Tier A bootstrap YAML (default: ./config.yaml)",
    )
    cfg_exp.add_argument(
        "--dsn",
        type=str,
        default=None,
        help="Override DB DSN instead of reading from --bootstrap",
    )
    cfg_exp.add_argument(
        "--revision",
        type=str,
        default=None,
        help="Revision UUID to export (default: the active revision)",
    )
    cfg_exp.add_argument(
        "--redact",
        action="store_true",
        help=(
            "Mask encrypted secrets as {encrypted: true, ...} markers "
            "(share-safe). Default emits inert ciphertext (round-trippable)."
        ),
    )

    # config show
    cfg_show = config_sub.add_parser(
        "show",
        help="Print a one-line summary of the active revision",
    )
    cfg_show.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
    )
    cfg_show.add_argument("--dsn", type=str, default=None)

    # config revisions
    cfg_rev = config_sub.add_parser(
        "revisions",
        help="List recent revisions (newest first)",
    )
    cfg_rev.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
    )
    cfg_rev.add_argument("--dsn", type=str, default=None)
    cfg_rev.add_argument("--limit", type=int, default=20)

    # config diff
    cfg_diff = config_sub.add_parser(
        "diff",
        help="Print a structural diff between two revisions",
    )
    cfg_diff.add_argument(
        "revision",
        type=str,
        help="Revision UUID to compare",
    )
    cfg_diff.add_argument(
        "--against",
        type=str,
        default="active",
        help="UUID or 'active' to diff against (default: active)",
    )
    cfg_diff.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
    )
    cfg_diff.add_argument("--dsn", type=str, default=None)

    # config seal
    cfg_seal = config_sub.add_parser(
        "seal",
        help=(
            "Encrypt the active revision's secrets under the Tier A keyring "
            "(one-time migration off the ${VAR} scheme)"
        ),
    )
    cfg_seal.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help="Path to the Tier A bootstrap YAML (supplies DSN + keyring)",
    )
    cfg_seal.add_argument("--dsn", type=str, default=None)
    cfg_seal.add_argument(
        "--summary",
        type=str,
        default=None,
        help="Audit summary for the sealed revision",
    )

    # config rekey
    cfg_rekey = config_sub.add_parser(
        "rekey",
        help=(
            "Re-encrypt the active revision's secrets under the active "
            "Tier A key (master-key rotation)"
        ),
    )
    cfg_rekey.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help="Path to the Tier A bootstrap YAML (supplies DSN + keyring)",
    )
    cfg_rekey.add_argument("--dsn", type=str, default=None)
    cfg_rekey.add_argument(
        "--summary",
        type=str,
        default=None,
        help="Audit summary for the rekeyed revision",
    )
    cfg_rekey.add_argument(
        "--dry-run",
        action="store_true",
        help="Report which secrets would re-encrypt; no DB write.",
    )

    # --- llm (subscription CLI backends) ---
    from crewlet.cli_llm import add_llm_parser

    add_llm_parser(sub)

    # --- secrets ---
    secrets_p = sub.add_parser(
        "secrets",
        help=(
            "Manage the Tier A encryption keyring and the encrypted "
            "secret store the engine resolves ${VAR} from"
        ),
    )
    secrets_sub = secrets_p.add_subparsers(dest="secrets_command")
    sec_keygen = secrets_sub.add_parser(
        "keygen",
        help="Generate a fresh keyring key + the config.yaml snippet to install it",
    )
    sec_keygen.add_argument(
        "--key-id",
        type=str,
        default="key-1",
        help="Key identifier stamped into every envelope this key seals",
    )

    def _store_flags(p: argparse.ArgumentParser) -> None:
        p.add_argument(
            "--bootstrap",
            type=Path,
            default=Path("config.yaml"),
            help="Path to the Tier A bootstrap YAML (supplies DSN + keyring)",
        )
        p.add_argument("--dsn", type=str, default=None)

    sec_set = secrets_sub.add_parser(
        "set",
        help="Store an encrypted secret the engine resolves ${NAME} from",
    )
    sec_set.add_argument("name", type=str, help="Environment-variable name")
    sec_set.add_argument(
        "--value",
        type=str,
        default=None,
        help=(
            "The secret value. Omit to read it from stdin (or be prompted "
            "on a terminal) — an argv value is visible in `ps` and lands in "
            "shell history."
        ),
    )
    sec_set.add_argument(
        "--source",
        type=str,
        default="cli",
        help="Provenance note recorded alongside the value (default: cli)",
    )
    _store_flags(sec_set)

    sec_list = secrets_sub.add_parser(
        "list",
        help="List stored secret names and metadata (never values)",
    )
    _store_flags(sec_list)

    sec_unset = secrets_sub.add_parser(
        "unset",
        help="Remove a stored secret",
    )
    sec_unset.add_argument("name", type=str, help="Environment-variable name")
    _store_flags(sec_unset)

    sec_get = secrets_sub.add_parser(
        "get",
        help="Print one stored secret value (requires --reveal)",
    )
    sec_get.add_argument("name", type=str, help="Environment-variable name")
    sec_get.add_argument(
        "--reveal",
        action="store_true",
        help=(
            "Required. Confirms you intend to print a plaintext secret to "
            "stdout; the read is logged."
        ),
    )
    _store_flags(sec_get)

    sec_rekey = secrets_sub.add_parser(
        "rekey",
        help="Re-encrypt stored secrets under the active keyring key",
    )
    sec_rekey.add_argument(
        "--dry-run",
        action="store_true",
        help="Report which secrets would re-encrypt; no DB write.",
    )
    _store_flags(sec_rekey)

    return parser


def _build_api_parser() -> argparse.ArgumentParser:
    """Parser for ``crewlet run api <config>``."""
    parser = argparse.ArgumentParser(
        prog="crewlet run api",
        description="Start the standalone API server.",
    )
    parser.add_argument("config", type=Path, help="Path to the YAML config file")
    parser.add_argument("--host", default="0.0.0.0", help="Bind host")
    parser.add_argument("--port", type=int, default=8000, help="Bind port")
    parser.add_argument("--debug", action="store_true", help="Enable debug logging")
    return parser


# ── Helpers ──────────────────────────────────────────────────────────


def _migration_vars(config: CompanyConfig) -> dict[str, str]:
    """Build template vars for SQL migrations from config.

    A config with no embeddings provider declares no width, so this
    returns an EMPTY dict and the migrator defers the pgvector
    migrations.  Guessing 1536 here would be the same permanent,
    irreversible corruption as guessing it for an unconfigured database:
    the column width is fixed at creation and the sequence is
    forward-only, so a founder who later adds a 3072-dimension model gets
    a diary and episode store that raise on every insert — silently,
    because ``ReflectEngine`` swallows the error.  Deferring costs
    nothing: an org with no embeddings provider is not using those tables
    yet, and the migrations apply the moment one is configured.
    """
    if config.providers.embeddings is None:
        return {}
    return {"embedding_dimensions": str(config.providers.embeddings.dimensions)}


def _migration_vars_for_payload(
    payload: dict[str, Any] | None, cipher: Any = None
) -> dict[str, str]:
    """Migration template vars derived from a raw Tier B payload.

    Returns an EMPTY dict when there is no active revision, which tells
    the migrator to stop before the pgvector migrations rather than guess
    a width.  This is the fix for a silent, permanent data-shape
    corruption: the width is baked into ``vector(N)`` at creation and the
    sequence is forward-only, so a guessed 1536 against a 3072-dimension
    model makes every ``agent_diary`` / ``episodes`` insert raise
    forever — and ``ReflectEngine`` swallows the error, so the learning
    subsystem just goes quiet.

    ``cipher`` decrypts an encrypted-document payload so the embedding
    dimensions can be read from the plaintext structure.
    """
    if payload is None:
        return {}
    from crewlet.secrets import load_config

    # Decrypt the stored config so the embedding dimension is readable.
    return _migration_vars(CompanyConfig.model_validate(load_config(payload, cipher)))


async def _connect_and_migrate_from_db(
    bootstrap: Any, *, import_company_path: Path | None = None
) -> tuple[Any, Any, Any, CompanyConfig | None]:
    """Connect, two-phase migrate, return
    ``(db, store, active_revision, import_company)``.

    Phase 1 applies only the ``company_config`` migration so the active
    revision (and thus the embedding dimensions) can be read BEFORE the
    pgvector migrations create their fixed-width vector columns.  Phase 2
    applies the rest with the resolved dimension.  Shared by ``cmd_run``
    and ``cmd_api``.

    ``import_company_path`` (``crewlet run --import-company``) is loaded
    and validated **only when there is no active revision** — so its
    embedding dimensions size the pgvector columns at first boot (the
    migrations are forward-only; the width can't change after they run),
    and so the flag is a true no-op on an already-configured engine: a
    later typo in the file must never break a restart that wouldn't read
    it.  The loaded :class:`CompanyConfig` is returned so the caller can
    insert it as the first revision; ``None`` when there's nothing to
    import (no path, or already configured).

    Also installs the process-wide **secret source** between the two
    phases, so every ``${VAR}`` resolved from here on — providers,
    transports, per-role MCP env, webhook secrets — sees the encrypted
    ``secret_values`` table before it falls back to ``os.environ``.  Both
    long-lived entry points (``run`` and ``api``) come through here, which
    is what keeps the engine and the API answering references identically.

    Both entry points may call this concurrently: the run is serialized
    behind an advisory lock, and the pgvector width is never guessed (an
    absent width defers those migrations instead), so neither process can
    win a race to bake the wrong column type.
    """
    from crewlet.db.client import Database
    from crewlet.db.company_config import CompanyConfigStore
    from crewlet.db.migrator import BOOTSTRAP_MIGRATIONS, migrate
    from crewlet.db.secret_values import load_secret_source
    from crewlet.secrets.resolver import install_secret_source

    if not bootstrap.providers.database.dsn:
        raise RuntimeError("providers.database.dsn is required in config.yaml")
    db = await Database.connect(bootstrap.providers.database.dsn)

    await migrate(db, only=set(BOOTSTRAP_MIGRATIONS))
    store = CompanyConfigStore(db)
    active = await store.get_active()

    from crewlet.secrets import KeyringCipher

    cipher = KeyringCipher.from_config(bootstrap.secrets)

    # Before the first ${VAR} of Tier B is resolved. Tier A is already
    # loaded and deliberately never consults the store (root of trust).
    install_secret_source(await load_secret_source(db, cipher))

    import_company: CompanyConfig | None = None
    if active is not None:
        if import_company_path is not None:
            logger.info(
                "import_company_ignored_already_configured",
                path=str(import_company_path),
            )
        template_vars = _migration_vars_for_payload(active.payload, cipher)
    elif import_company_path is not None:
        from crewlet.config import load_company_config

        import_company = load_company_config(import_company_path)
        template_vars = _migration_vars(import_company)
    else:
        template_vars = _migration_vars_for_payload(None)

    await migrate(db, template_vars=template_vars)
    return db, store, active, import_company


async def _import_company_for_run(
    store: Any,
    company: CompanyConfig,
    company_config_path: Path,
    cipher: Any = None,
) -> Any:
    """Write ``company`` as the first active revision and return it.

    Used by ``crewlet run --import-company`` to give first-time users a
    single-command bootstrap.  The caller (:func:`_connect_and_migrate_from_db`)
    guarantees no active revision exists and has already loaded +
    validated the YAML during the boot migrate (so a bad file aborts
    before the engine spawns, and the pgvector columns were sized from
    this config's embedding dimensions).

    The config is encrypted under ``cipher`` (the Tier A keyring) before
    the revision is stored; ``None`` stores it as plaintext (no keyring).
    """
    from crewlet.config_yaml import company_config_to_dict
    from crewlet.secrets import store_config

    payload = store_config(company_config_to_dict(company), cipher)
    revision_id = await store.insert_active(
        payload,
        created_by="cli",
        source="cli.run.--import-company",
        summary=f"bootstrap from {company_config_path.name}",
        parent_revision_id=None,
    )
    logger.info(
        "company_imported_at_run",
        revision_id=str(revision_id),
        path=str(company_config_path),
    )
    print(f"Imported {company_config_path} as revision {revision_id}")
    return await store.get_active()


def _build_api_event_store(db: Any | None) -> Any:
    """Build the event store for the standalone API process.

    Wraps the persistent leg in a ``CompositeEventStore`` so that
    ``/agents`` aggregates per-agent token totals via the composite's
    ``list_token_usage_events`` path.  Per-store ``get_agent_states``
    deliberately leaves token counters at zero, so wiring
    ``BufferedEventStore`` directly would make the API dashboard
    report zero tokens.

    The memory leg accumulates the webhook trace events that
    ``routes._log_event`` writes via ``event_store.write_event``
    (the composite's ``write_event`` dual-writes both legs), capped at
    ``MemoryEventStore.max_events``.  The API process does NOT receive
    engine events, so the only token-bearing data lives in the
    persistent leg — token aggregation effectively reads from there
    via ``list_token_usage_events`` deduped by ``event_id``.

    When ``db`` is ``None`` (e.g. in tests that don't spin up a real
    database), falls back to two in-memory legs — still wrapped in a
    composite so the per-store wiring stays consistent.
    """
    from crewlet.timescaledb import (
        BufferedEventStore,
        CompositeEventStore,
        MemoryEventStore,
        TimescaleDBEventStore,
    )

    if db is not None:
        persistent = BufferedEventStore(TimescaleDBEventStore(db))
        return CompositeEventStore(persistent, MemoryEventStore())

    return CompositeEventStore(MemoryEventStore(), MemoryEventStore())


def _build_engine_event_store(memory_store: Any, persistent_store: Any | None) -> Any:
    """Wrap the engine's per-run stores in a ``CompositeEventStore``.

    ``cmd_run`` registers two publish listeners when a persistent
    store is available — one writes to ``memory_store`` (fast,
    in-process), one writes to ``persistent_store`` (durable, backed
    by TimescaleDB).  The composite merges both legs for reads and
    handles cross-store dedup via ``list_token_usage_events``.

    When no persistent store is available, the memory leg is the sole
    source of events — but we still wrap it in a composite so that
    per-agent token totals are aggregated via
    ``list_token_usage_events`` instead of via the per-store
    ``get_agent_states`` (which deliberately leaves tokens at
    zero).  The satellite memory leg in that fallback is empty.
    """
    from crewlet.timescaledb import CompositeEventStore, MemoryEventStore

    if persistent_store is not None:
        return CompositeEventStore(persistent_store, memory_store)
    return CompositeEventStore(memory_store, MemoryEventStore())


# ── Commands ──────────────────────────────────────────────────────────


def cmd_run(args: argparse.Namespace) -> int:
    """Load Tier A from ``config.yaml``, connect to DB, read the active
    Tier B revision from ``company_config``, and run the engine.

    When no active revision exists the engine boots into the
    unconfigured state and serves the API so an operator can
    bootstrap via ``crewlet config import`` or ``PUT /config``.
    """

    from crewlet._env import load_env_file
    from crewlet.config import load_bootstrap_config
    from crewlet.engine import Engine

    # Load .env next to the bootstrap file, falling back to CWD
    config_path: Path = args.config
    load_env_file(config_path)

    level = logging.DEBUG if args.debug else logging.INFO
    configure_logging(level=level)
    # Optional one-shot Confluence import — runs BEFORE the Tier A
    # bootstrap is loaded because the import is self-contained: its
    # Confluence credentials come from the Tier B --import-company YAML
    # (its ``confluence:`` block), never from the Tier A bootstrap. Gating
    # it behind ``config.yaml`` would make ``crewlet run --import-company X
    # --import-confluence Y`` silently skip the import whenever the Tier A
    # file is missing or invalid — even though the import needs neither.
    # Publishing first also matches the documented "publish skills +
    # knowledge docs, then serve" one-invocation deploy flow; failure here
    # aborts before engine start so the operator never gets a half-seeded
    # company.
    if args.import_confluence is not None:
        # The import publishes to Confluence, whose credentials live in
        # the Tier B company config (its ``confluence:`` block) -- not the
        # Tier A bootstrap passed as the positional ``config``.  Source
        # the company config from --import-company.
        if args.import_company is None:
            print(
                "Error: --import-confluence requires --import-company so the "
                "Confluence integration is read from the Tier B company "
                "config (the Tier A bootstrap has no 'confluence' block). "
                "Or publish separately: `crewlet confluence import "
                "<company.yaml>`.",
                file=sys.stderr,
            )
            return 1
        confluence_company_path: Path = args.import_company
        if not confluence_company_path.exists():
            print(
                f"Error: --import-company file not found: {confluence_company_path}",
                file=sys.stderr,
            )
            return 1
        import_path: Path = args.import_confluence
        if not import_path.exists():
            print(
                f"Error: --import-confluence path not found: {import_path}",
                file=sys.stderr,
            )
            return 1
        if import_path.is_dir():
            md_files = list(import_path.rglob("*.md"))
        elif import_path.suffix == ".md":
            md_files = [import_path]
        else:
            md_files = []
        if not md_files:
            print(
                f"Error: --import-confluence path has no .md files: {import_path}",
                file=sys.stderr,
            )
            return 1
        from crewlet.confluence.import_cli import _resolve_skill_space, run_import
        from crewlet.confluence.pages import ConfluencePublishError

        skill_space = _resolve_skill_space(None)
        try:
            asyncio.run(
                run_import(
                    config_path=confluence_company_path,
                    paths=[import_path],
                    update=bool(args.update_confluence),
                    dry_run=False,
                    create_space=bool(args.create_confluence_space),
                    prune=bool(args.prune_confluence),
                    skill_space=skill_space,
                )
            )
        except ConfluencePublishError as exc:
            print(f"Confluence import failed: {exc}", file=sys.stderr)
            return 1
        except (RuntimeError, ValueError) as exc:
            print(f"Error during Confluence import: {exc}", file=sys.stderr)
            return 1

    # Optional one-shot Plane import — the exact analog of the
    # Confluence block above (argparse guarantees at most one of the two
    # flags is present): self-contained, credentials from the Tier B
    # --import-company YAML ('integrations.plane'), runs before the
    # Tier A bootstrap load, aborts engine start on failure.
    if args.import_plane is not None:
        if args.import_company is None:
            print(
                "Error: --import-plane requires --import-company so the "
                "Plane integration is read from the Tier B company config "
                "(the Tier A bootstrap has no 'plane' block). Or publish "
                "separately: `crewlet plane import <company.yaml>`.",
                file=sys.stderr,
            )
            return 1
        plane_company_path: Path = args.import_company
        if not plane_company_path.exists():
            print(
                f"Error: --import-company file not found: {plane_company_path}",
                file=sys.stderr,
            )
            return 1
        plane_import_path: Path = args.import_plane
        if not plane_import_path.exists():
            print(
                f"Error: --import-plane path not found: {plane_import_path}",
                file=sys.stderr,
            )
            return 1
        if plane_import_path.is_dir():
            plane_md_files = list(plane_import_path.rglob("*.md"))
        elif plane_import_path.suffix == ".md":
            plane_md_files = [plane_import_path]
        else:
            plane_md_files = []
        if not plane_md_files:
            print(
                f"Error: --import-plane path has no .md files: {plane_import_path}",
                file=sys.stderr,
            )
            return 1
        from crewlet.plane.import_cli import (
            PlanePublishError,
            _resolve_skills_project,
        )
        from crewlet.plane.import_cli import (
            run_import as plane_run_import,
        )

        try:
            asyncio.run(
                plane_run_import(
                    config_path=plane_company_path,
                    paths=[plane_import_path],
                    update=bool(args.update_plane),
                    dry_run=False,
                    prune=bool(args.prune_plane),
                    skills_project=_resolve_skills_project(None),
                )
            )
        except PlanePublishError as exc:
            print(f"Plane import failed: {exc}", file=sys.stderr)
            return 1
        except (RuntimeError, ValueError) as exc:
            print(f"Error during Plane import: {exc}", file=sys.stderr)
            return 1

    # Tier A bootstrap is required only to *run the engine*, so it is
    # loaded after the optional self-contained knowledge import above.
    if not config_path.exists():
        print(
            f"Error: bootstrap config not found: {config_path} "
            f"(pass a path or place config.yaml in the cwd)",
            file=sys.stderr,
        )
        return 1

    try:
        bootstrap = load_bootstrap_config(config_path)
    except Exception as exc:
        print(f"Error: invalid bootstrap config: {exc}", file=sys.stderr)
        return 1

    try:
        _bind_node_identity(bootstrap)
    except ValueError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1

    async def _run_engine() -> None:
        from crewlet.a2a.memory import MemoryA2ABus
        from crewlet.queue.pulsar import PulsarEventQueue

        queue_cfg = bootstrap.providers.queue
        event_queue: Any = PulsarEventQueue(
            queue_cfg.url,
            tenant=queue_cfg.tenant,
            namespace=queue_cfg.namespace,
            auth_token=queue_cfg.auth_token,
            tls_trust_certs_path=queue_cfg.tls_trust_certs_path,
        )
        a2a_bus = MemoryA2ABus()

        # Connect + two-phase migrate.  When --import-company is given
        # and the DB has no active revision, the file is loaded inside
        # (only then) so its embedding dimensions size the pgvector
        # columns AND so the flag stays a true no-op on an already-
        # configured engine — a later typo in the file must never break
        # a restart that wouldn't read it.
        (
            db,
            company_config_store,
            active_revision,
            import_company,
        ) = await _connect_and_migrate_from_db(
            bootstrap, import_company_path=args.import_company
        )
        storage: Any = db

        # Secret-encryption keyring (Tier A ``secrets``).  Encrypts the
        # imported company below and decrypts the embeddings provider's
        # api_key, which is built here (outside apply_config) for the
        # learning subsystem.
        from crewlet.secrets import KeyringCipher, load_config

        cipher = KeyringCipher.from_config(bootstrap.secrets)

        # One-command bootstrap: import the (just-loaded) config as the
        # first revision so ``crewlet run config.yaml --import-company
        # company.yaml`` is a single command instead of an
        # `import + run` two-step.
        if import_company is not None and active_revision is None:
            active_revision = await _import_company_for_run(
                company_config_store,
                import_company,
                args.import_company,
                cipher,
            )

        # Embedding provider for the learning subsystem (agent_diary,
        # episodes).  Required when an active revision exists -- those
        # pgvector stores embed their content; Engine.start() wires
        # them once the Database is up.  If no active revision exists,
        # the engine boots unconfigured and waits for PUT /config.
        embeddings: Any = None
        if active_revision is not None:
            from crewlet.config import CompanyConfig, create_embedding_provider

            # Decrypt the stored document so the embeddings api_key is a
            # real value.
            active_cfg = CompanyConfig.model_validate(
                load_config(active_revision.payload, cipher)
            )
            if active_cfg.providers.embeddings is not None:
                embeddings = create_embedding_provider(active_cfg.providers.embeddings)

        engine = Engine.from_bootstrap(
            bootstrap,
            event_queue=event_queue,
            a2a_bus=a2a_bus,
            storage=storage,
            embeddings=embeddings,
            company_config_store=company_config_store,
        )

        if args.debug:
            engine.debug = True

        # CLI --api-port overrides bootstrap config
        if args.api_port:
            engine._api_port = args.api_port

        # If a Tier B revision was already active when the engine
        # started, apply it now so the spawn cascade runs during
        # ``engine.run()``.
        if active_revision is not None:
            from crewlet.config import CompanyConfig

            # Decrypt the stored config to plaintext before apply.
            await engine.apply_config(
                CompanyConfig.model_validate(
                    load_config(active_revision.payload, cipher)
                )
            )

        # Event store setup — in-memory store for instant reads,
        # TimescaleDB for persistence across restarts.  The event
        # store is always wrapped in a ``CompositeEventStore`` by
        # ``_build_engine_event_store`` so per-agent token totals are
        # aggregated via ``list_token_usage_events``.  Both
        # legs share the same PostgreSQL instance as the rest of
        # Crewlet — there is no separate observability database.
        from crewlet.timescaledb import (
            EventStoreWriter,
            MemoryEventStore,
            TimescaleDBEventStore,
        )

        memory_store = MemoryEventStore()
        await memory_store.start()
        memory_writer = EventStoreWriter(memory_store)
        event_queue.add_publish_listener(memory_writer.on_publish)

        persistent_store = TimescaleDBEventStore(db)
        await persistent_store.start()
        persistent_writer = EventStoreWriter(persistent_store)
        event_queue.add_publish_listener(persistent_writer.on_publish)

        engine._event_store = _build_engine_event_store(memory_store, persistent_store)

        try:
            await engine.run()
        finally:
            await persistent_store.close()
            await memory_store.close()

    print(f"Starting crewlet engine from {config_path} …")
    try:
        asyncio.run(_run_engine())
    except KeyboardInterrupt:
        _print_safe("\nShutdown requested (Ctrl+C).")
    except asyncio.CancelledError:
        # asyncio.run() may surface CancelledError instead of
        # KeyboardInterrupt when SIGINT arrives before the engine
        # installs its own signal handlers (e.g. during DB/Pulsar
        # connect).
        _print_safe("\nShutdown requested (Ctrl+C).")
    except (RuntimeError, ValueError, FileNotFoundError) as exc:
        # FileNotFoundError covers a missing --import-company file (loaded
        # lazily inside _connect_and_migrate_from_db only when the engine
        # is unconfigured); ValueError covers an invalid one.
        _print_safe(f"Error: {exc}")
        return 1
    return 0


def _detect_tier(path: Path) -> str:
    """Guess which config tier ``path`` holds.

    Deterministic rather than heuristic: ``name`` is the one **required**
    Tier B field, and Tier A forbids it (both models are
    ``extra="forbid"``), so its presence separates the two exactly.  An
    unreadable / non-mapping file is reported as ``company`` so the
    caller surfaces the real parse error instead of a tier mismatch.
    """
    try:
        raw = yaml.safe_load(path.read_text())
    except Exception:
        return "company"
    if isinstance(raw, dict) and "name" not in raw:
        return "bootstrap"
    return "company"


def _validation_errors(exc: Exception) -> list[dict[str, str]]:
    """Flatten an exception into ``{path, message, type}`` records.

    A :class:`pydantic.ValidationError` carries one entry per offending
    field with its exact location, which is what makes an automated
    write -> validate -> fix loop converge; anything else (a YAML syntax
    error, an org-model ``ValueError`` such as an unknown unit lead)
    becomes a single pathless record.
    """
    from pydantic import ValidationError

    if isinstance(exc, ValidationError):
        return [
            {
                "path": ".".join(str(p) for p in err["loc"]),
                "message": err["msg"],
                "type": err["type"],
            }
            for err in exc.errors()
        ]
    return [{"path": "", "message": str(exc), "type": type(exc).__name__}]


def _validate_config_file(path: Path, tier: str) -> dict[str, Any]:
    """Validate one config file and return a structured result.

    The single validation routine behind both ``crewlet validate``
    output modes, so the human text and the ``--json`` payload can never
    disagree about whether a file is valid.

    No environment is read: Tier B keeps ``${VAR}`` references verbatim
    (see :func:`~crewlet.config.load_company_config`), so a config
    validates fully before any secret exists — which is what lets an
    editor, CI job, or AI authoring loop check a draft offline.
    """
    from crewlet.config import (
        config_to_organization,
        load_bootstrap_config,
        load_company_config,
    )

    result: dict[str, Any] = {"valid": False, "tier": tier, "errors": []}

    try:
        if tier == "bootstrap":
            bootstrap = load_bootstrap_config(path)
            result["summary"] = {
                "api_port": bootstrap.api.port,
                "queue": bootstrap.providers.queue.type,
                "database": bool(bootstrap.providers.database.dsn),
            }
        else:
            config = load_company_config(path)
            # Building the Organization runs the org-model validators
            # (lead references, schedule cron/timezone/target, etc.), so
            # a bad schedule fails here cleanly rather than at fire time.
            org = config_to_organization(config)
            result["summary"] = {
                "name": config.name,
                "units": len(org.all_units()),
                "roles": len(org.all_roles()),
                "agent_seats": sum(1 for r in org.all_roles() if not r.is_human),
                "human_seats": sum(1 for r in org.all_roles() if r.is_human),
                "llm_providers": sorted(config.providers.llm),
                "mcp_servers": [s.name for s in config.mcp_servers],
            }
    except Exception as exc:
        result["errors"] = _validation_errors(exc)
        return result

    result["valid"] = True
    return result


def cmd_validate(args: argparse.Namespace) -> int:
    """Validate a Tier A or Tier B YAML config, reporting any errors."""
    config_path: Path = args.config
    as_json: bool = getattr(args, "as_json", False)

    if not config_path.exists():
        if as_json:
            print(
                json.dumps(
                    {
                        "valid": False,
                        "tier": getattr(args, "tier", "auto"),
                        "errors": [
                            {
                                "path": "",
                                "message": f"config file not found: {config_path}",
                                "type": "FileNotFoundError",
                            }
                        ],
                    },
                    indent=2,
                )
            )
        else:
            print(f"Error: config file not found: {config_path}", file=sys.stderr)
        return 1

    tier = getattr(args, "tier", "auto")
    if tier == "auto":
        tier = _detect_tier(config_path)

    result = _validate_config_file(config_path, tier)

    if as_json:
        print(json.dumps(result, indent=2))
        return 0 if result["valid"] else 1

    if not result["valid"]:
        print(f"Config invalid ({result['tier']} tier):", file=sys.stderr)
        for err in result["errors"]:
            where = err["path"] or "<root>"
            print(f"  {where}: {err['message']}", file=sys.stderr)
        return 1

    summary = result["summary"]
    if result["tier"] == "bootstrap":
        print(f"Config OK: Tier A bootstrap ({config_path})")
        print(f"  Queue       : {summary['queue']}")
        print(f"  Database    : {'configured' if summary['database'] else 'none'}")
        print(f"  API port    : {summary['api_port']}")
        return 0

    print(f"Config OK: {summary['name']}")
    print(f"  Units       : {summary['units']}")
    print(f"  Roles       : {summary['roles']}")
    if summary["llm_providers"]:
        print(f"  LLM         : {', '.join(summary['llm_providers'])}")
    return 0


#: JSON Schema dialect the generated documents declare.  Pydantic v2
#: emits 2020-12 constructs (``$defs``, ``prefixItems``), and the
#: yaml-language-server / editor tooling that consumes these files keys
#: off ``$schema`` to pick its validator.
_JSON_SCHEMA_DIALECT = "https://json-schema.org/draft/2020-12/schema"

#: Canonical location of each generated schema — the URL an editor's
#: ``# yaml-language-server: $schema=…`` modeline, a CI linter, or an AI
#: assistant working without crewlet installed will fetch.
#:
#: Publishing these two files at this prefix is a deployment step: the
#: docs site must serve ``schema/*.schema.json`` from the repository.
#: Until it does, the raw repository URL
#: (``raw.githubusercontent.com/crewlet/crewlet/main/schema/…``) serves
#: the identical files — a ``$id`` names a document, it does not have to
#: be the address you fetched it from.
_SCHEMA_ID_BASE = "https://docs.crewlet.ai/schema"

_SCHEMA_IDS = {
    "company": f"{_SCHEMA_ID_BASE}/company.schema.json",
    "bootstrap": f"{_SCHEMA_ID_BASE}/bootstrap.schema.json",
}


def build_config_schema(tier: str) -> dict[str, Any]:
    """Return the JSON Schema for one config tier.

    Generated from the Pydantic models, so it cannot drift from what the
    loader actually accepts — which is the point: it is the authoritative
    field list for editors (via a ``# yaml-language-server: $schema=…``
    modeline), for CI, and for AI-assisted authoring, none of which
    should be inferring the surface from prose docs.  ``extra="forbid"``
    throughout means the schema also carries
    ``additionalProperties: false``, so a mistyped key is flagged in the
    editor rather than at boot.
    """
    from crewlet.config import BootstrapConfig, CompanyConfig

    model = {"company": CompanyConfig, "bootstrap": BootstrapConfig}[tier]
    schema = model.model_json_schema()
    # Prepend the identity keys; dict order drives the emitted JSON.
    return {
        "$schema": _JSON_SCHEMA_DIALECT,
        "$id": _SCHEMA_IDS[tier],
        **schema,
    }


def _bind_node_identity(bootstrap: Any) -> str:
    """Resolve this process's node id and bind it onto every log line.

    Called by the long-lived entry points right after the Tier A config
    loads.  With one process it is a constant label; with more than one it
    is the difference between "the config apply failed" and "the config
    apply failed on node-2".
    """
    import structlog

    from crewlet.config import resolve_node_id

    node_id = resolve_node_id(bootstrap)
    structlog.contextvars.bind_contextvars(node=node_id)
    return node_id


def cmd_migrate(args: argparse.Namespace) -> int:
    """Apply pending database migrations (the explicit schema step).

    Recommended before starting any long-lived process.  ``crewlet run``
    still auto-migrates for the single-host flow, but that convenience is
    the thing that made three entry points race an unlocked, non-idempotent
    DDL sequence — running this once, first, removes the race entirely.
    """

    async def _run() -> int:
        from crewlet._env import load_env_file
        from crewlet.config import load_bootstrap_config
        from crewlet.db.client import Database
        from crewlet.db.migrator import (
            BOOTSTRAP_MIGRATIONS,
            migrate,
            pending_migrations,
        )

        # Same as every other entry point: Tier A resolves ${VAR} at load
        # time, and the shipped examples put the DSN in
        # ${CREWLET_DATABASE_DSN}. Without this, the first command the
        # deployment guide tells an operator to run fails on a config
        # `crewlet run` in the same directory loads fine.
        load_env_file(args.config)
        bootstrap = load_bootstrap_config(args.config)
        if not bootstrap.providers.database.dsn:
            print(
                "Error: providers.database.dsn is required in the Tier A config",
                file=sys.stderr,
            )
            return 1

        db = await Database.connect(bootstrap.providers.database.dsn)
        try:
            # The bootstrap phase is dimension-independent and must exist
            # before the active revision can be read at all.
            if not args.check:
                await migrate(db, only=set(BOOTSTRAP_MIGRATIONS))

            template_vars = await _resolve_migration_vars(db, bootstrap, args.company)

            if args.check:
                pending = await pending_migrations(db, template_vars=template_vars)
                if pending:
                    print(f"{len(pending)} pending migration(s):", file=sys.stderr)
                    for version in pending:
                        print(f"  {version}", file=sys.stderr)
                    return 1
                print("schema up to date", file=sys.stderr)
                return 0

            applied = await migrate(db, template_vars=template_vars)
            if applied:
                print(f"applied {len(applied)} migration(s):", file=sys.stderr)
                for version in applied:
                    print(f"  {version}", file=sys.stderr)
            else:
                print("schema up to date", file=sys.stderr)

            remaining = await pending_migrations(db, template_vars=template_vars)
            if remaining:
                print(
                    f"\n{len(remaining)} migration(s) still pending: they size "
                    f"their pgvector columns from the embedding width, which "
                    f"no active company config declares yet.\nImport a company "
                    f"(`crewlet config import company.yaml`) or pass "
                    f"`--company company.yaml`, then re-run.",
                    file=sys.stderr,
                )
            return 0
        finally:
            await db.close()

    return asyncio.run(_run())


async def _resolve_migration_vars(
    db: Any, bootstrap: Any, company_path: Path | None
) -> dict[str, str]:
    """Migration template vars from the active revision, or ``--company``.

    Returns an empty dict when neither is available — the migrator then
    stops before the pgvector migrations rather than guessing a width.

    Tolerates a database where ``company_config`` does not exist yet.
    ``--check`` deliberately applies nothing, including the bootstrap
    phase, so on a fresh database this read would otherwise raise
    ``UndefinedTableError`` — a raw traceback in exactly the case the
    flag exists for.
    """
    from crewlet.db.company_config import CompanyConfigStore
    from crewlet.secrets import KeyringCipher

    cipher = KeyringCipher.from_config(bootstrap.secrets)
    active = None
    if await db.fetchval("SELECT to_regclass('company_config')"):
        active = await CompanyConfigStore(db).get_active()
    if active is not None:
        return _migration_vars_for_payload(active.payload, cipher)
    if company_path is not None:
        from crewlet.config import load_company_config

        return _migration_vars(load_company_config(company_path))
    return {}


async def _open_budget_store(args: argparse.Namespace) -> tuple[Any, Any]:
    """Connect and return ``(db, PostgresBudgetUsageStore)``."""
    from crewlet._env import load_env_file
    from crewlet.config import load_bootstrap_config
    from crewlet.db.budgets import PostgresBudgetUsageStore
    from crewlet.db.client import Database

    load_env_file(args.config)
    bootstrap = load_bootstrap_config(args.config)
    if not bootstrap.providers.database.dsn:
        raise RuntimeError("providers.database.dsn is required in the Tier A config")
    db = await Database.connect(bootstrap.providers.database.dsn)
    return db, PostgresBudgetUsageStore(db)


def cmd_budgets_show(args: argparse.Namespace) -> int:
    """Print token usage per scope."""

    async def _run() -> int:
        db, _store = await _open_budget_store(args)
        try:
            rows = await db.execute(
                "SELECT scope, used_tokens, updated_at FROM token_budget_usage "
                "ORDER BY scope"
            )
        finally:
            await db.close()
        if not rows:
            print("no token usage recorded", file=sys.stderr)
            return 0
        width = max(len(str(r["scope"])) for r in rows)
        for row in rows:
            print(f"{str(row['scope']):<{width}}  {int(row['used_tokens']):>12,}")
        return 0

    try:
        return asyncio.run(_run())
    except Exception as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


def cmd_budgets_reset(args: argparse.Namespace) -> int:
    """Zero token usage.

    Usage is durable across restarts — it used to reset on every engine
    start, which made a cap advisory in exactly the situation that
    motivates one (an agent burning budget in a crash loop). Resetting is
    therefore a deliberate act rather than a side effect of a restart.
    """

    async def _run() -> int:
        db, store = await _open_budget_store(args)
        try:
            count = await store.reset(args.scope)
        finally:
            await db.close()
        target = args.scope or "all scopes"
        print(f"reset {count} scope(s) ({target})", file=sys.stderr)
        return 0

    try:
        return asyncio.run(_run())
    except Exception as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1


def cmd_schema(args: argparse.Namespace) -> int:
    """Print (or write) the JSON Schema for a config tier."""
    text = json.dumps(build_config_schema(args.tier), indent=2) + "\n"

    output: Path | None = args.output
    if output is None:
        sys.stdout.write(text)
        return 0

    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(text)
    print(f"Wrote {args.tier} schema to {output}", file=sys.stderr)
    return 0


def cmd_api(args: argparse.Namespace) -> int:
    """Start the standalone API server (Tier A bootstrap + Tier B from DB)."""

    from crewlet._env import load_env_file
    from crewlet.config import load_bootstrap_config

    config_path: Path = args.config

    # Load .env next to the bootstrap file, falling back to CWD
    load_env_file(config_path)

    level = logging.DEBUG if args.debug else logging.INFO
    configure_logging(level=level)

    if not config_path.exists():
        print(
            f"Error: bootstrap config not found: {config_path}",
            file=sys.stderr,
        )
        return 1

    try:
        bootstrap = load_bootstrap_config(config_path)
    except Exception as exc:
        print(f"Error: invalid bootstrap config: {exc}", file=sys.stderr)
        return 1

    try:
        _bind_node_identity(bootstrap)
    except ValueError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 1

    # Queue backend from Tier A
    from crewlet.queue.pulsar import PulsarEventQueue

    queue_cfg = bootstrap.providers.queue
    event_queue = PulsarEventQueue(
        queue_cfg.url,
        tenant=queue_cfg.tenant,
        namespace=queue_cfg.namespace,
        auth_token=queue_cfg.auth_token,
        tls_trust_certs_path=queue_cfg.tls_trust_certs_path,
    )

    try:
        from crewlet.api.app import attach_config_refresh, create_app
        from crewlet.api.streaming import StreamService
    except ImportError:
        print(
            "Error: crewlet[api] extras required. "
            "Install with: pip install crewlet[api]",
            file=sys.stderr,
        )
        return 1

    async def _run_api() -> None:
        import uvicorn

        # Connect + migrate. Safe to run concurrently with the engine:
        # the run takes an advisory lock, and the pgvector width comes
        # from the active revision or defers — it is never guessed, which
        # is what made this call race the engine for the right to bake
        # the wrong column type.
        db, company_config_store, _active, _ic = await _connect_and_migrate_from_db(
            bootstrap
        )

        # Event store: the helper wraps the persistent leg in a
        # CompositeEventStore so /agents aggregates per-agent token
        # totals (see _build_api_event_store).
        event_store = _build_api_event_store(db)

        stream = StreamService()

        # ``app.state.agent_roles`` / ``org_data`` / ``tools_data``
        # / ``github_webhook_secret`` / ``forge_app_id`` are all
        # populated by ``attach_config_refresh`` after the app starts
        # (from the active revision, then refreshed live on every
        # ``ConfigRevisionActivated`` event) — the same derivation the
        # embedded API uses.
        #
        # The OTLP receiver is built here too. In a split deployment THIS
        # is the externally-reachable process, so it is what
        # CREWLET_SANDBOX_OTEL_RECEIVER_URL names; omitting it answered
        # 503 to every trace, metric and log a detached coding run
        # exported. Tokens are signed, so the engine mints and this
        # process verifies without sharing memory.
        from crewlet.sandbox.otel import build_sandbox_otel_receiver

        app = create_app(
            event_queue=event_queue,
            event_store=event_store,
            database=db,
            stream=stream,
            bootstrap=bootstrap,
            company_config_store=company_config_store,
            sandbox_otel_receiver=build_sandbox_otel_receiver(bootstrap),
        )

        await event_store.start()
        await event_queue.start()
        # Engine emits events on ``crewlet.events.>``.  Subscribe with
        # an ephemeral broadcast consumer so every API instance gets
        # every event without competing with peers; ``ingest`` updates
        # the live-state projection and fans the event out to dashboards.
        stream_unsubscribe = await event_queue.subscribe_stream(
            "crewlet.events.>", stream.ingest
        )
        # Subscribe to revision_activated + prime app.state from the
        # active revision (if any).  After this, the dashboard reads
        # /agents and /org against the up-to-date Tier B payload, and
        # any subsequent PUT /config refreshes app.state in place.
        await attach_config_refresh(app)

        try:
            uv_config = uvicorn.Config(
                app,
                host=args.host,
                port=args.port,
                log_level="debug" if args.debug else "info",
            )
            server = uvicorn.Server(uv_config)
            await server.serve()
        finally:
            await stream_unsubscribe()
            await event_queue.stop()
            await event_store.close()
            await db.close()

    print(f"Starting crewlet API from {config_path} on {args.host}:{args.port} …")
    try:
        asyncio.run(_run_api())
    except KeyboardInterrupt:
        _print_safe("\nShutdown requested (Ctrl+C).")
    except (RuntimeError, ValueError) as exc:
        _print_safe(f"Error: {exc}")
        return 1
    return 0


# ── Entry point ───────────────────────────────────────────────────────


def main(argv: list[str] | None = None) -> int:
    if argv is None:
        argv = sys.argv[1:]

    # Intercept `crewlet run api ...` before argparse sees it
    if len(argv) >= 2 and argv[0] == "run" and argv[1] == "api":
        api_parser = _build_api_parser()
        args = api_parser.parse_args(argv[2:])
        return cmd_api(args)

    parser = _build_parser()
    args = parser.parse_args(argv)

    # Configure logging to stderr up front so read commands (e.g.
    # ``config export``) never mix structlog lines into their stdout
    # payload. Idempotent — ``cmd_run`` / ``cmd_api`` re-call it with a
    # level and the guard keeps the first win.
    configure_logging(
        level=logging.DEBUG if getattr(args, "debug", False) else logging.INFO
    )

    if args.command is None:
        parser.print_help()
        return 0

    if args.command == "budgets":
        subcmd = getattr(args, "budgets_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        return {
            "show": cmd_budgets_show,
            "reset": cmd_budgets_reset,
        }[subcmd](args)

    if args.command == "confluence":
        from crewlet.confluence.import_cli import (
            cmd_confluence_import,
            cmd_confluence_resync,
        )

        subcmd = getattr(args, "confluence_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        confluence_commands = {
            "import": cmd_confluence_import,
            "resync": cmd_confluence_resync,
        }
        return confluence_commands[subcmd](args)

    if args.command == "plane":
        from crewlet.plane.import_cli import cmd_plane_import, cmd_plane_resync
        from crewlet.plane.provision_cli import cmd_plane_provision

        subcmd = getattr(args, "plane_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        plane_commands = {
            "import": cmd_plane_import,
            "resync": cmd_plane_resync,
            "provision": cmd_plane_provision,
        }
        return plane_commands[subcmd](args)

    if args.command == "mattermost":
        from crewlet.mattermost.provision_cli import (
            cmd_mattermost_doctor,
            cmd_mattermost_provision,
        )

        subcmd = getattr(args, "mattermost_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        mattermost_commands = {
            "doctor": cmd_mattermost_doctor,
            "provision": cmd_mattermost_provision,
        }
        return mattermost_commands[subcmd](args)

    if args.command == "gitlab":
        from crewlet.gitlab.provision_cli import cmd_gitlab_provision

        subcmd = getattr(args, "gitlab_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        gitlab_commands = {
            "provision": cmd_gitlab_provision,
        }
        return gitlab_commands[subcmd](args)

    if args.command == "slack":
        from crewlet.slack.provision_cli import cmd_slack_provision

        subcmd = getattr(args, "slack_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        slack_commands = {"provision": cmd_slack_provision}
        return slack_commands[subcmd](args)

    if args.command == "config":
        from crewlet.cli_config_handlers import (
            cmd_config_diff,
            cmd_config_export,
            cmd_config_import,
            cmd_config_rekey,
            cmd_config_revisions,
            cmd_config_seal,
            cmd_config_show,
        )

        subcmd = getattr(args, "config_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        config_commands = {
            "import": cmd_config_import,
            "export": cmd_config_export,
            "show": cmd_config_show,
            "revisions": cmd_config_revisions,
            "diff": cmd_config_diff,
            "seal": cmd_config_seal,
            "rekey": cmd_config_rekey,
        }
        return config_commands[subcmd](args)

    if args.command == "llm":
        from crewlet.cli_llm import LLM_COMMANDS

        subcmd = getattr(args, "llm_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        return LLM_COMMANDS[subcmd](args)

    if args.command == "secrets":
        from crewlet.cli_config_handlers import (
            cmd_secrets_get,
            cmd_secrets_keygen,
            cmd_secrets_list,
            cmd_secrets_rekey,
            cmd_secrets_set,
            cmd_secrets_unset,
        )

        subcmd = getattr(args, "secrets_command", None)
        if subcmd is None:
            parser.print_help()
            return 0
        secrets_commands = {
            "keygen": cmd_secrets_keygen,
            "set": cmd_secrets_set,
            "list": cmd_secrets_list,
            "unset": cmd_secrets_unset,
            "get": cmd_secrets_get,
            "rekey": cmd_secrets_rekey,
        }
        return secrets_commands[subcmd](args)

    commands = {
        "run": cmd_run,
        "validate": cmd_validate,
        "migrate": cmd_migrate,
        "schema": cmd_schema,
    }

    return commands[args.command](args)


def _entry_point() -> None:
    """Console-script entry point (propagates exit code)."""
    raise SystemExit(main())


if __name__ == "__main__":
    _entry_point()
