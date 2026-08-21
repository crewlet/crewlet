"""Shared token-minting plumbing for the provisioning CLIs.

The integration-agnostic ``${VAR}``-minting contract — the config's own
references are the contract; sinks are where minted values leave the
process. Each provisioner (GitLab, Plane, ...) scans its integration's
config block for ``${VAR}`` references via :func:`referenced_env_vars`,
mints credentials into those vars, and hands the values to a
:class:`TokenSink`. A var that already has a value is considered
provisioned — the APIs never return a credential after creation, which is
what makes minting idempotent.

Three sinks ship:

- :class:`SecretStoreSink` — the encrypted ``secret_values`` table. The
  engine reads it back directly, so nothing has to be sourced into a
  shell or restarted into place.
- :class:`EnvFileSink` — an env file the operator sources.
- :class:`PrintSink` — ``export VAR=token`` lines on stdout.

``record``, ``discard`` and ``flush`` are **async** because a sink may
persist remotely. That also makes write-through affordable: a minted
credential is unretrievable from the upstream API, so the window between
"minted" and "persisted" is a window in which a crash orphans a live
credential. Every sink here persists inside ``record``; ``flush`` is the
closing safety net, not the moment the write happens. ``discard`` is the
other half — a credential revoked because it could not be persisted
everywhere must not be left standing in the vars that *were* written,
since a dead token reads exactly like a live one.

Integration-specific seat scans (e.g. GitLab's conventional
``sandbox.env.GITLAB_TOKEN`` key) stay in the integration's own
``provision`` module; only the generic pieces live here.
"""

from __future__ import annotations

import bisect
import os
from pathlib import Path
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.env_file import (
    format_assignment,
    is_exported,
    parse_assignment,
    write_secret_file,
)
from crewlet.env_refs import env_var_reference
from crewlet.env_refs import referenced_env_vars as _referenced_env_vars

logger = get_logger("provisioning")


def referenced_env_vars(mapping: dict[str, str]) -> list[str]:
    """``${VAR}`` names referenced in a config block's values, in order."""
    return _referenced_env_vars(mapping.values())


def sole_env_var(value: str) -> str:
    """The ``${VAR}`` name when *value* is EXACTLY one whole reference.

    Returns ``""`` for a literal, an embedded reference
    (``"prefix-${VAR}"``) or a multi-reference value (``"${A}${B}"``) —
    the shapes a capture-into-one-var contract (e.g. Plane's
    server-generated webhook secret) can never satisfy, because the
    resolved value would be a concatenation, not the captured secret.
    """
    return env_var_reference(value)


class TokenSink(Protocol):
    """Where minted token values are written."""

    def existing(self, var: str) -> str:
        """Current value of ``var`` (empty ⇒ needs minting).

        Synchronous: every sink pre-loads what it knows at construction,
        so the minting loop can ask without awaiting.
        """
        ...

    async def record(self, var: str, token: str) -> None:
        """Persist a freshly minted/rotated token value.

        Write-through — when this returns, the value has survived the
        process.
        """
        ...

    async def discard(self, var: str) -> None:
        """Undo a :meth:`record` — leave ``var`` carrying no value here.

        The other half of write-through, and it exists for one shape: a
        credential referenced from several ``${VAR}``s, persisted one at
        a time, where a failure partway leaves some already written.  The
        only safe response to a value that cannot be finished is to
        revoke it upstream — and a revoke on its own leaves every var
        already written holding a token that no longer authenticates,
        which nothing downstream can tell from a good one.

        Write-through as well: when this returns, the value is gone from
        wherever this sink keeps it.  Discarding a var this sink has no
        value for is a no-op, not an error.
        """
        ...

    async def flush(self) -> None:
        """Closing safety net; persisting already happened in `record`."""
        ...


class EnvFileSink:
    """Append/update ``VAR=value`` lines in an env file.

    A seat's ``${VAR}`` is considered already-provisioned when the file
    (or the process env) already carries a non-empty value for it, so
    re-runs never re-mint — the API won't return the token again anyway.

    ``export VAR=value`` lines (the shape ``--print`` emits, so pasted
    ``--print`` output round-trips) are understood: the key is indexed
    without the prefix, values may be single- or double-quoted, and an
    update rewrites the line in place *keeping* the ``export`` prefix.

    The file is held at ``0600`` — applied at creation, and re-applied on
    every write, because the common case is an env file the operator
    already made by hand under the default umask. Every ``record`` writes
    through, so a crash mid-run can never leave a minted-but-unrecorded
    credential.
    """

    def __init__(self, path: str) -> None:
        self._path = path
        self._lines: list[str] = []
        # EVERY line assigning a key, not just the last one.  An env file
        # may legitimately carry a key twice (an operator appended a
        # line, two runs wrote through different paths), and a shell
        # takes the LAST assignment — so a map holding only one index
        # cannot say which lines are shadowing which.  That is invisible
        # while values only ever get rewritten, and wrong the moment one
        # is removed: dropping the effective line promotes the shadowed
        # one, turning a discard into a downgrade to an older secret.
        self._index: dict[str, list[int]] = {}
        self._exported: set[str] = set()
        self._existed = os.path.exists(path)
        self._dirty = False
        if self._existed:
            with open(path, encoding="utf-8") as fh:
                self._lines = fh.read().splitlines()
            for i, line in enumerate(self._lines):
                parsed = parse_assignment(line)
                if parsed is None:
                    continue
                key, _value = parsed
                if is_exported(line):
                    self._exported.add(key)
                self._index.setdefault(key, []).append(i)

    def existing(self, var: str) -> str:
        for index in reversed(self._index.get(var, [])):
            # Last assignment first: that is the one a shell would apply.
            parsed = parse_assignment(self._lines[index])
            if parsed is not None and parsed[1]:
                return parsed[1]
        return os.environ.get(var, "")

    async def record(self, var: str, token: str) -> None:
        # Quoted per the shared grammar, not interpolated bare: a token
        # holding a space makes ``source`` fail on the line AND abandon
        # every credential below it, and one holding ``${...}`` is
        # rewritten by the python-dotenv reader the engine boots with.
        line = format_assignment(var, token, export=var in self._exported)
        indexes = self._index.get(var, [])
        if indexes:
            # Write the effective (last) line and DELETE the ones it was
            # already shadowing.  Each of those holds a superseded
            # credential, in a secrets file, for as long as the file
            # lives — and this module does not leave dead credentials
            # lying around anywhere else either.
            self._lines[indexes[-1]] = line
            self._drop_lines(indexes[:-1])
        else:
            self._index[var] = [len(self._lines)]
            self._lines.append(line)
        self._dirty = True
        # Write-through: a minted credential is unretrievable from the
        # upstream API, so it must not live only in this list.
        self._write()

    async def discard(self, var: str) -> None:
        """Drop ``var``'s lines entirely, rather than blanking them.

        A blank ``VAR=`` would leave a dead credential's name standing in
        a secrets file for no reason, and a shell sourcing it would still
        set the variable — to the empty string, shadowing nothing but
        claiming the name. Removing the lines leaves the file saying
        nothing about the var, which is the truth.

        *Every* line for the key goes, not just the effective one:
        removing only the last would promote a shadowed assignment, so a
        discard meant to erase a dead token would instead hand the shell
        an older one.

        What this canNOT undo is a value already exported in the calling
        shell: :meth:`existing` falls back to ``os.environ``, so an
        operator who ran ``source .env`` before the run still has the
        dead token in their environment and the next run in that same
        shell will read the var as provisioned. Removing the line is
        necessary, not sufficient — which is why the caller that needs
        this guarantee (Mattermost's token rollback) also revokes the
        credential upstream, where no shell can resurrect it.
        """
        indexes = self._index.get(var, [])
        if not indexes:
            return
        self._drop_lines(indexes)
        self._exported.discard(var)
        self._dirty = True
        self._write()

    def _drop_lines(self, indexes: list[int]) -> None:
        """Remove these line numbers and reindex what is left."""
        doomed = set(indexes)
        if not doomed:
            return
        self._lines = [line for i, line in enumerate(self._lines) if i not in doomed]
        shift = sorted(doomed)
        rebuilt: dict[str, list[int]] = {}
        for key, positions in self._index.items():
            kept = [
                i - bisect.bisect_left(shift, i) for i in positions if i not in doomed
            ]
            if kept:
                rebuilt[key] = kept
        self._index = rebuilt

    async def flush(self) -> None:
        # A run that recorded nothing must not conjure a file into
        # existence (the flush-on-abort wrapper reaches here on every
        # preflight abort); an already-existing file is rewritten as-is.
        if not self._dirty and not self._existed:
            return
        self._write()

    def _write(self) -> None:
        """Rewrite the file — owner-only, atomically.

        Two properties, and neither came free.

        **The mode is the new file's.** ``O_CREAT`` without ``O_EXCL``
        ignores the mode argument for a file that already exists, so an
        ``.env`` the operator made by hand — ``touch``, an editor,
        ``0644`` under the default umask, and this sink's default target
        — kept that mode while every minted token was written into it.

        **There is no truncated middle.** The whole file is rewritten on
        every ``record``, so a crash or a full disk part-way through
        destroyed the credentials already minted in the same run, which
        the API will not issue again.

        Replacing the inode carries both: the new file is created 0600
        before a byte is written, and a reader sees either the old file
        or the new one. See :func:`crewlet.env_file.write_secret_file`.
        """
        write_secret_file(Path(self._path), "\n".join(self._lines) + "\n")


class PrintSink:
    """Print ``export VAR=token`` for each minted token; env-only reads.

    Write-through like every other sink in this module, and for the same
    reason: a minted token is live on its account the moment it is
    returned and its value is never retrievable again, so anything that
    holds it back until a later flush loses it outright if the run dies
    in between.  Buffering also made this the one sink that broke the
    module's own stated invariant — "every sink here persists inside
    ``record``".  For this sink "persist" is the operator reading it, so
    the line is printed and flushed as it is minted; the report is
    written to stdout too, so it interleaves, which is the honest
    ordering rather than a tidy one that can be lost.

    It keeps nothing afterwards. The written line IS the record, so a
    list of everything this sink has ever minted would be a second copy
    of every token, held for the life of the process, that nothing reads
    back.
    """

    def existing(self, var: str) -> str:
        return os.environ.get(var, "")

    async def record(self, var: str, token: str) -> None:
        # ``flush=True`` is what makes the write-through above true.
        # Python line-buffers stdout only when it is a terminal; piped to
        # a file or captured by CI it is block-buffered, so an operator
        # running ``--print > secrets.env`` loses every line still in the
        # buffer if the process is killed — exactly the window this sink
        # prints eagerly to close.
        print(format_assignment(var, token, export=True), flush=True)

    async def discard(self, var: str) -> None:
        """Print the shell counterpart of the line that was printed.

        ``unset`` is what makes the emitted stream correct rather than
        merely appended-to: an operator who piped ``--print`` to a file
        and sources it gets a var with no value, which is the state that
        makes the next run re-mint — not the dead token the earlier
        ``export`` line put there.
        """
        print(f"unset {var}", flush=True)

    async def flush(self) -> None:
        return None


class SecretStoreSink:
    """Write minted values into the encrypted ``secret_values`` table.

    The sink that closes the provisioning loop: the engine resolves
    ``${VAR}`` from this same table, so a minted credential is live
    without an env file to source or a shell to re-export. Values are
    sealed with the Tier A keyring on the way in.

    Existing values are pre-loaded once at construction (via
    :meth:`prime`) so :meth:`existing` stays synchronous — and the
    already-provisioned check keeps working across sinks, since a var
    already carrying a value in the environment still counts as minted.
    """

    def __init__(self, store: Any, *, updated_by: str, source: str) -> None:
        self._store = store
        self._updated_by = updated_by
        self._source = source
        self._known: dict[str, str] = {}

    async def prime(self) -> None:
        """Load what the store already holds, so re-runs never re-mint."""
        self._known = await self._store.load_all()

    def existing(self, var: str) -> str:
        stored = self._known.get(var, "")
        if stored:
            return stored
        return os.environ.get(var, "")

    async def record(self, var: str, token: str) -> None:
        await self._store.put(
            var, token, updated_by=self._updated_by, source=self._source
        )
        self._known[var] = token
        logger.info("secret_store_sink_recorded", var=var, source=self._source)

    async def discard(self, var: str) -> None:
        await self._store.delete(var)
        self._known.pop(var, None)
        logger.info("secret_store_sink_discarded", var=var, source=self._source)

    async def flush(self) -> None:
        """No-op — :meth:`record` already committed each value."""
        return


def add_sink_arguments(parser: Any, *, default_env_file: str) -> None:
    """Register the sink-selection flags shared by every provisioning CLI.

    One definition so ``gitlab provision`` and ``plane provision`` cannot
    drift into offering different destinations for the same kind of
    minted credential.
    """
    from pathlib import Path

    parser.add_argument(
        "--secret-store",
        action="store_true",
        help=(
            "Write minted credentials into the encrypted secret_values "
            "table instead of an env file. The engine resolves ${VAR} from "
            "there directly, so nothing has to be sourced into a shell. "
            "Requires a Tier A keyring and DB DSN (--bootstrap / --dsn)."
        ),
    )
    parser.add_argument(
        "--env-file",
        type=Path,
        default=Path(default_env_file),
        help=(
            f"Env file to append/update minted tokens into "
            f"(default: {default_env_file}). Ignored with --secret-store."
        ),
    )
    parser.add_argument(
        "--print",
        dest="print_tokens",
        action="store_true",
        help="Print 'export VAR=token' lines to stdout instead of writing an env file",
    )
    parser.add_argument(
        "--bootstrap",
        type=Path,
        default=Path("config.yaml"),
        help=(
            "Path to the Tier A bootstrap YAML (supplies the DB DSN + "
            "encryption keyring for --secret-store)"
        ),
    )
    parser.add_argument(
        "--dsn",
        type=str,
        default=None,
        help="Override DB DSN instead of reading from --bootstrap",
    )


async def open_token_sink(args: Any, *, source: str) -> tuple[TokenSink, Any | None]:
    """Build the sink the CLI flags select, plus a DB handle to close.

    Returns ``(sink, db)``; ``db`` is ``None`` for the file and stdout
    sinks. ``--print`` wins over ``--secret-store`` so a dry inspection
    never writes anywhere.
    """
    if getattr(args, "print_tokens", False):
        return PrintSink(), None
    if not getattr(args, "secret_store", False):
        return EnvFileSink(str(args.env_file)), None

    from pathlib import Path

    from crewlet.config import load_bootstrap_config
    from crewlet.db.client import Database
    from crewlet.db.secret_values import SecretStoreError, SecretValueStore
    from crewlet.secrets import KeyringCipher

    dsn = getattr(args, "dsn", None) or ""
    cipher = None
    bootstrap_path = Path(getattr(args, "bootstrap", "config.yaml"))
    if bootstrap_path.exists():
        bootstrap = load_bootstrap_config(bootstrap_path)
        dsn = dsn or bootstrap.providers.database.dsn
        cipher = KeyringCipher.from_config(bootstrap.secrets)
    if not dsn:
        raise SecretStoreError(
            "--secret-store needs a database DSN — pass --dsn or point "
            f"--bootstrap at a Tier A config (looked at {bootstrap_path})"
        )
    if cipher is None:
        raise SecretStoreError(
            "--secret-store needs an encryption keyring — set Tier A "
            "`secrets.keys` in the bootstrap config (generate one with "
            "`crewlet secrets keygen`). Secrets are never stored unencrypted."
        )
    from crewlet.db.secret_values import load_secret_source
    from crewlet.secrets.resolver import install_secret_source

    db = await Database.connect(dsn)
    sink = SecretStoreSink(
        SecretValueStore(db, cipher), updated_by=f"cli:{source}", source=source
    )
    await sink.prime()
    # Install the snapshot for THIS process too. The provisioners resolve
    # their integration block's ${VAR}s to validate it, and a re-run must
    # see what a previous run stored — otherwise a signing secret already
    # in the table reads as unset and the config fails validation for a
    # credential that is, in fact, provisioned.
    install_secret_source(await load_secret_source(db, cipher))
    return sink, db


__all__ = [
    "EnvFileSink",
    "PrintSink",
    "SecretStoreSink",
    "TokenSink",
    "add_sink_arguments",
    "open_token_sink",
    "referenced_env_vars",
    "sole_env_var",
]
