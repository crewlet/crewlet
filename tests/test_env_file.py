"""The shared ``.env`` assignment grammar.

A provisioning run mints credentials the upstream API will not hand
out twice, so the only thing that matters about the file they land in
is that what comes back out is what went in — through the python-dotenv
reader ``crewlet run`` boots with, and through an operator's ``source``.

Both writers used to answer that separately: the Slack provisioner
reasoned the quoting through, and the shared ``TokenSink`` env-file
backend wrote ``f"{var}={token}"``. These pin the one grammar.
"""

from __future__ import annotations

import os
import re
import subprocess
from pathlib import Path

import pytest
from dotenv import dotenv_values

from crewlet.env_file import (
    EnvValueError,
    format_assignment,
    is_exported,
    parse_assignment,
)
from crewlet.provisioning import EnvFileSink

# Values a credential could plausibly carry, each of which breaks a
# bare ``KEY=value`` line in at least one of the two readers.
AWKWARD = [
    "plain-token-123",
    "tok en",  # a space ends the assignment for `source`
    "tok#en",  # `#` starts a comment
    'tok"en',
    "tok'en",
    "tok\\en",
    "tok$en",  # python-dotenv expands `$NAME`
    "a b#c'd\"e\\f",
]


# ── the grammar itself ──


@pytest.mark.parametrize("value", AWKWARD)
def test_python_dotenv_reads_back_exactly_what_was_written(value, tmp_path):
    """The reader that matters: ``crewlet run``'s ``load_env_file``."""
    path = tmp_path / ".env"
    path.write_text(format_assignment("TOKEN", value) + "\n")
    assert dotenv_values(path)["TOKEN"] == value


@pytest.mark.parametrize("value", AWKWARD)
def test_a_shell_reads_back_exactly_what_was_written(value, tmp_path):
    """The other reader: an operator sourcing the file.

    A bare space is not merely wrong for one value — ``source`` fails on
    that line and abandons every assignment BELOW it, so one awkward
    token silently empties the rest of the credential file.
    """
    path = tmp_path / "env.sh"
    path.write_text(
        format_assignment("TOKEN", value, export=True)
        + "\nexport SENTINEL=still-here\n"
    )
    out = subprocess.run(
        [
            "bash",
            "-c",
            f'set -e; source "{path}"; printf "%s\\n%s" "$TOKEN" "$SENTINEL"',
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    read_token, sentinel = out.stdout.split("\n")
    assert read_token == value
    assert sentinel == "still-here", "a later assignment was lost"


@pytest.mark.parametrize("value", AWKWARD)
def test_parse_assignment_is_the_exact_inverse_of_format(value):
    line = format_assignment("TOKEN", value)
    assert parse_assignment(line) == ("TOKEN", value)


def test_the_export_prefix_round_trips():
    line = format_assignment("TOKEN", "tok en", export=True)
    assert is_exported(line)
    assert parse_assignment(line) == ("TOKEN", "tok en")
    assert not is_exported(format_assignment("TOKEN", "x"))


def test_an_unrepresentable_value_is_refused_not_mangled():
    """``${...}`` is interpolated in EVERY python-dotenv quoting form.

    There is no way to store it faithfully, so the write fails loudly
    rather than persisting a value that reads back different.
    """
    with pytest.raises(EnvValueError, match=r"\$\{\.\.\.\}"):
        format_assignment("TOKEN", "abc${HOME}def")
    with pytest.raises(EnvValueError, match="single quote"):
        format_assignment("TOKEN", "a$b'c")


def test_a_non_assignment_line_parses_to_nothing():
    assert parse_assignment("# a comment") is None
    assert parse_assignment("") is None
    assert parse_assignment("not an assignment") is None
    assert parse_assignment("9INVALID=x") is None


# ── the sink that writes through it ──


@pytest.mark.parametrize("value", AWKWARD)
async def test_the_env_file_sink_round_trips_a_minted_token(value, tmp_path):
    """The minted-credential path end to end, through BOTH readers.

    An unretrievable token that reads back changed is the same outage as
    one that was never persisted, and harder to see. The sink writes the
    file the engine boots from AND the file an operator sources, so it
    has to satisfy both.
    """
    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    await sink.record("PLANE_API_KEY", value)
    await sink.record("SECOND_SECRET", "still-here")
    await sink.flush()

    assert dotenv_values(path)["PLANE_API_KEY"] == value
    out = subprocess.run(
        [
            "bash",
            "-c",
            f'set -e; source "{path}"; '
            'printf "%s\\n%s" "$PLANE_API_KEY" "$SECOND_SECRET"',
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    sourced, sentinel = out.stdout.split("\n")
    assert sourced == value
    assert sentinel == "still-here", (
        "sourcing failed on the awkward line and abandoned the credential "
        "written after it"
    )
    # And a re-run must see it as already-provisioned, so it never re-mints.
    assert EnvFileSink(str(path)).existing("PLANE_API_KEY") == value


async def test_the_sink_refuses_a_token_it_cannot_store_faithfully(tmp_path):
    """``${...}`` is expanded by the reader in every quoting form.

    Persisting it silently would hand the engine a different credential
    than the one the API minted, and the API will not issue it again.
    """
    sink = EnvFileSink(str(tmp_path / ".env"))
    with pytest.raises(EnvValueError):
        await sink.record("PLANE_API_KEY", "abc${HOME}def")


async def test_the_sink_keeps_the_export_prefix_when_updating(tmp_path):
    path = tmp_path / ".env"
    path.write_text("export PLANE_API_KEY=old\n")
    sink = EnvFileSink(str(path))
    await sink.record("PLANE_API_KEY", "new one")
    assert path.read_text().startswith("export PLANE_API_KEY=")
    assert dotenv_values(path)["PLANE_API_KEY"] == "new one"


async def test_the_sink_does_not_fall_through_to_a_stale_shell_export(
    tmp_path, monkeypatch
):
    """The file is the durable store; a sourced shell holds last run's value."""
    monkeypatch.setenv("PLANE_API_KEY", "stale-from-a-previous-source")
    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    await sink.record("PLANE_API_KEY", "fresh")
    assert EnvFileSink(str(path)).existing("PLANE_API_KEY") == "fresh"
    await sink.discard("PLANE_API_KEY")
    # Discarded: nothing in the file, so the process env is all that is
    # left — and the next run re-mints rather than trusting it.
    assert "PLANE_API_KEY" not in dotenv_values(path)


def test_the_sink_creates_the_file_owner_only(tmp_path):
    path = tmp_path / ".env"
    EnvFileSink(str(path))._write()
    assert path.stat().st_mode & 0o777 == 0o600


async def test_a_minted_token_never_lands_in_a_world_readable_file(tmp_path):
    """The operator almost always made the `.env` first.

    `os.open(path, ..., 0o600)` applies the mode only on CREATION, so
    writing into a hand-made file (0644 under the usual umask, and the
    default `--env-file` target of several provisioning CLIs) left every
    minted PAT readable by any user on the host.
    """
    path = tmp_path / ".env"
    path.write_text("EXISTING=value\n")
    path.chmod(0o644)

    sink = EnvFileSink(str(path))
    await sink.record("PLANE_API_KEY", "minted-secret")

    assert path.stat().st_mode & 0o777 == 0o600
    assert dotenv_values(path)["EXISTING"] == "value"


async def test_a_failed_write_cannot_destroy_already_minted_credentials(tmp_path):
    """The whole file is rewritten on every `record`.

    A crash or a full disk partway through a truncate-and-write would
    leave a half-file — losing credentials minted earlier in the same
    run, which the API will not issue again. The write is atomic, so a
    reader sees the old file or the new one.
    """
    import os as _os

    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    await sink.record("FIRST_TOKEN", "first-value")

    real_replace = _os.replace

    def boom(src, dst):
        raise OSError(28, "No space left on device")

    _os.replace = boom
    try:
        with pytest.raises(OSError):
            await sink.record("SECOND_TOKEN", "second-value")
    finally:
        _os.replace = real_replace

    assert dotenv_values(path)["FIRST_TOKEN"] == "first-value"
    assert not list(tmp_path.glob(".*.tmp")), "the temp file was left behind"


# ── one grammar, structurally ──


#: Modules that build a ``key=value`` string which is NOT an env-file
#: line, so the quoting rules do not apply and applying them would be
#: the bug.
_ASSIGNMENT_EXEMPT = {
    # argv elements handed straight to ``docker -e`` / ``podman -e``.
    # No shell parses these, so a quoted value would arrive WITH its
    # quotes and the sandbox would see a different environment.
    "sandbox/local.py": "container -e argv, never parsed by a shell",
    # A process-local digest of what ${VAR} references resolve to.
    # Never written anywhere, never read by a shell.
    "config_resolution.py": "resolution fingerprint, not a file",
    # ``node.labels`` rendered for a lease's metadata column.
    "seat/placement.py": "label rendering, not env vars",
}


def test_no_module_respells_the_env_assignment_grammar():
    """No module may write its own ``KEY=value`` line.

    The divergence this replaced put quoting in one provisioner and not
    the other, so whether a minted credential survived depended on which
    CLI minted it. Guarded structurally so a third writer fails here —
    which is how ``PrintSink`` and the keyring keygen snippet, both
    emitting ``export VAR=…`` for an operator to source, were found.
    """
    root = Path(__file__).resolve().parents[1] / "src" / "crewlet"
    # An f-string that builds an assignment from a variable — the shape
    # every hand-rolled writer had.
    pattern = re.compile(r"""f["'][^"']*\{[A-Za-z_.\[\]'"]+\}=\{""")
    offenders = sorted(
        str(path.relative_to(root))
        for path in root.rglob("*.py")
        if path.name != "env_file.py"
        and str(path.relative_to(root)) not in _ASSIGNMENT_EXEMPT
        and pattern.search(path.read_text())
    )
    assert offenders == [], (
        "these modules build their own KEY=value lines instead of using "
        f"crewlet.env_file.format_assignment: {offenders}"
    )


def test_the_assignment_regex_is_defined_once():
    root = Path(__file__).resolve().parents[1] / "src" / "crewlet"
    pattern = re.compile(r"re\.compile\(\s*r?[\"'][^\"']*export")
    offenders = sorted(
        str(path.relative_to(root))
        for path in root.rglob("*.py")
        if path.name != "env_file.py" and pattern.search(path.read_text())
    )
    assert offenders == [], (
        f"these modules compile their own assignment-line regex: {offenders}"
    )


def test_os_environ_is_still_the_documented_fallback(tmp_path, monkeypatch):
    monkeypatch.setenv("NEVER_WRITTEN", "from-the-shell")
    assert EnvFileSink(str(tmp_path / ".env")).existing("NEVER_WRITTEN") == (
        "from-the-shell"
    )
    assert os.environ["NEVER_WRITTEN"] == "from-the-shell"
