"""Shared ``.env`` loading for CLI entry points.

Every CLI command that resolves credentials from the process environment
— ``run``, ``api``, ``confluence import`` / ``resync``, ``plane import``
/ ``resync`` / ``provision``, and ``gitlab provision`` — loads a project
``.env`` the same way through :func:`load_env_file`.

Keeping this in one place means the standalone integration commands
honour ``.env`` exactly like ``run`` does, so the credential-error hint
("Set it in your environment (or .env) before publishing pages") is
actually true for every path that emits it — and the provisioning CLIs
resolve their connection ``${VAR}``s (``url`` / ``workspace`` /
operator-token fallbacks) from the same ``.env``.  Re-run idempotency
for MINTED values is owned elsewhere: ``EnvFileSink`` reads its own
``--env-file`` (default ``.env.plane`` / ``.env.gitlab``), which this
loader never touches.
"""

from __future__ import annotations

from pathlib import Path

from dotenv import load_dotenv


def env_file_for(config_path: Path) -> Path:
    """The ``.env`` file a command invoked with *config_path* uses.

    A ``.env`` beside *config_path* wins; otherwise ``./.env`` in the
    working directory. The path is returned whether or not it exists —
    writers create it, readers skip a missing file — so both sides can
    name the same file before it is there.

    One function owns this decision because writers and readers must not
    choose independently. When they did, a provisioner could write beside
    the company YAML while ``crewlet run`` loaded a ``.env`` beside the
    Tier A bootstrap; every ``${VAR}`` then resolved to empty at boot with
    no error at all.

    Args:
        config_path: The YAML config the command was invoked with; its
            directory is the primary place to look for a sibling ``.env``.
    """
    sibling = config_path.parent / ".env"
    return sibling if sibling.exists() else Path(".env")


def load_env_file(config_path: Path) -> None:
    """Load the ``.env`` for *config_path* into the process environment.

    ``override=False`` so real environment variables always win over the
    file. A missing ``.env`` is a no-op — the environment is used as-is.
    """
    env_file = env_file_for(config_path)
    if env_file.exists():
        load_dotenv(env_file, override=False)


__all__ = ["env_file_for", "load_env_file"]
