"""Direct unit tests for the shared provisioning sink/scan module.

These pin the sink contracts directly — read precedence,
append-vs-update, flush semantics, and the ``${VAR}`` reference scan —
since both the GitLab and Plane provisioners build on the exact same
behavior.
"""

from __future__ import annotations

import os

from crewlet.provisioning import (
    EnvFileSink,
    PrintSink,
    SecretStoreSink,
    referenced_env_vars,
    sole_env_var,
)

# ── referenced_env_vars ──


def test_referenced_env_vars_extracts_in_order():
    mapping = {
        "token": "${TOKEN_A}",
        "other": "${TOKEN_B}",
    }
    assert referenced_env_vars(mapping) == ["TOKEN_A", "TOKEN_B"]


def test_referenced_env_vars_embedded_and_multiple_refs():
    # Refs embedded inside a larger value are found, including several in
    # one value, in encounter order.
    mapping = {"auth": "Bearer ${TOKEN_A}:${TOKEN_B}", "url": "https://x/${TOKEN_C}/y"}
    assert referenced_env_vars(mapping) == ["TOKEN_A", "TOKEN_B", "TOKEN_C"]


def test_referenced_env_vars_dedupes_keeping_first_position():
    mapping = {
        "a": "${SHARED}",
        "b": "${OTHER}",
        "c": "${SHARED}",
    }
    assert referenced_env_vars(mapping) == ["SHARED", "OTHER"]


def test_referenced_env_vars_ignores_literals_and_non_strings():
    assert referenced_env_vars({"a": "literal", "b": 42, "c": None}) == []
    assert referenced_env_vars({}) == []


def test_referenced_env_vars_ignores_malformed_refs():
    # Only the ${WORD} form counts — $VAR, ${}, and ${dashed-name} do not.
    assert referenced_env_vars({"a": "$BARE", "b": "${}", "c": "${no-dash}"}) == []


# ── sole_env_var ──


def test_sole_env_var_accepts_exactly_one_whole_reference():
    assert sole_env_var("${PLANE_WEBHOOK_SECRET}") == "PLANE_WEBHOOK_SECRET"
    # Surrounding whitespace is tolerated (YAML scalars often carry it).
    assert sole_env_var("  ${TOKEN}  ") == "TOKEN"


def test_sole_env_var_rejects_literals_embedded_and_multi_refs():
    # A capture contract needs the WHOLE value to be the reference — an
    # embedded or concatenated form resolves to something the captured
    # secret can never equal.
    assert sole_env_var("") == ""
    assert sole_env_var("whsec_literal") == ""
    assert sole_env_var("wh-${SECRET}") == ""
    assert sole_env_var("${SECRET}-suffix") == ""
    assert sole_env_var("${A}${B}") == ""


# ── EnvFileSink ──


async def test_env_file_sink_missing_file(tmp_path, monkeypatch):
    # A nonexistent path is fine: reads fall through to os.environ and
    # record+flush creates the file.
    monkeypatch.delenv("MISSING_VAR", raising=False)
    monkeypatch.setenv("FROM_ENV", "env-value")
    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    assert not path.exists()
    assert sink.existing("MISSING_VAR") == ""
    assert sink.existing("FROM_ENV") == "env-value"
    await sink.record("NEW_VAR", "minted")
    await sink.flush()
    assert path.read_text() == "NEW_VAR=minted\n"


def test_env_file_sink_existing_prefers_file_over_environ(monkeypatch, tmp_path):
    # A non-empty file value wins over os.environ.
    monkeypatch.setenv("TOKEN_X", "from-environ")
    path = tmp_path / ".env"
    path.write_text("TOKEN_X=from-file\n")
    sink = EnvFileSink(str(path))
    assert sink.existing("TOKEN_X") == "from-file"


def test_env_file_sink_empty_file_value_falls_back_to_environ(monkeypatch, tmp_path):
    # A present-but-empty line (VAR=) does NOT count as provisioned; the
    # process env is the fallback, and "" when that is unset too.
    monkeypatch.setenv("TOKEN_Y", "from-environ")
    monkeypatch.delenv("TOKEN_Z", raising=False)
    path = tmp_path / ".env"
    path.write_text("TOKEN_Y=\nTOKEN_Z=\n")
    sink = EnvFileSink(str(path))
    assert sink.existing("TOKEN_Y") == "from-environ"
    assert sink.existing("TOKEN_Z") == ""


async def test_env_file_sink_record_appends_new_and_updates_in_place(tmp_path):
    # Comments and unrelated lines are preserved; an existing var is
    # rewritten on its own line, a new var is appended at the end.
    path = tmp_path / ".env"
    path.write_text("# header comment\nKEEP=old\nOTHER=untouched\n")
    sink = EnvFileSink(str(path))
    await sink.record("KEEP", "new")
    await sink.record("ADDED", "fresh")
    await sink.flush()
    assert path.read_text() == (
        "# header comment\nKEEP=new\nOTHER=untouched\nADDED=fresh\n"
    )


async def test_env_file_sink_flush_round_trip(tmp_path, monkeypatch):
    # Values flushed by one sink are visible to a fresh sink on the same
    # path — the re-run idempotency contract.
    monkeypatch.delenv("ROUND_TRIP", raising=False)
    path = tmp_path / ".env"
    first = EnvFileSink(str(path))
    await first.record("ROUND_TRIP", "value-1")
    await first.flush()
    second = EnvFileSink(str(path))
    assert second.existing("ROUND_TRIP") == "value-1"


async def test_env_file_sink_flush_without_records_rewrites_content(tmp_path):
    # Flushing with nothing recorded keeps the file's lines intact.
    path = tmp_path / ".env"
    path.write_text("# comment\nA=1\n")
    sink = EnvFileSink(str(path))
    await sink.flush()
    assert path.read_text() == "# comment\nA=1\n"


async def test_env_file_sink_flush_without_records_writes_no_new_file(tmp_path):
    # A preflight abort reaches the flush-on-abort wrapper with nothing
    # recorded; on a nonexistent path that must NOT conjure a stray env
    # file — a file holding a single blank line reads
    # like it holds credentials.
    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    await sink.flush()
    assert not path.exists()
    # Recording afterwards creates the file immediately (write-through).
    await sink.record("VAR", "value")
    assert path.read_text() == "VAR=value\n"
    await sink.flush()
    assert path.read_text() == "VAR=value\n"


async def test_env_file_sink_understands_export_lines(tmp_path, monkeypatch):
    # `export VAR=value` is the exact shape --print emits — pasting that
    # output into the env file must read back as provisioned (no re-mint,
    # no duplicate key) and updates keep the export prefix in place.
    monkeypatch.delenv("PLANE_TOKEN_SWE", raising=False)
    path = tmp_path / ".env"
    path.write_text("# pasted from --print\nexport PLANE_TOKEN_SWE=abc\nPLAIN=x\n")
    sink = EnvFileSink(str(path))
    assert sink.existing("PLANE_TOKEN_SWE") == "abc"
    assert sink.existing("PLAIN") == "x"
    await sink.record("PLANE_TOKEN_SWE", "rotated")
    await sink.flush()
    assert path.read_text() == (
        "# pasted from --print\nexport PLANE_TOKEN_SWE=rotated\nPLAIN=x\n"
    )
    # Round-trip: a fresh sink still reads the exported form.
    assert EnvFileSink(str(path)).existing("PLANE_TOKEN_SWE") == "rotated"


def test_env_file_sink_strips_quotes_when_reading(tmp_path, monkeypatch):
    # Hand-edited env files often quote values; the sink reads through
    # matching single or double quotes (a quoted value is provisioned).
    monkeypatch.delenv("QUOTED_D", raising=False)
    monkeypatch.delenv("QUOTED_S", raising=False)
    path = tmp_path / ".env"
    path.write_text("QUOTED_D=\"dq-value\"\nexport QUOTED_S='sq-value'\n")
    sink = EnvFileSink(str(path))
    assert sink.existing("QUOTED_D") == "dq-value"
    assert sink.existing("QUOTED_S") == "sq-value"


async def test_env_file_sink_record_is_write_through(tmp_path):
    # record() persists immediately — a minted credential is unretrievable
    # from the upstream API, so it must survive a crash before flush(). A
    # fresh sink on the same path reads it back with no flush in between.
    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    await sink.record("PENDING", "already-persisted")
    assert sink.existing("PENDING") == "already-persisted"
    assert path.read_text() == "PENDING=already-persisted\n"
    assert EnvFileSink(str(path)).existing("PENDING") == "already-persisted"


async def test_env_file_sink_creates_file_owner_only(tmp_path):
    # Minted PATs must never be world-readable. The mode is applied at
    # CREATION (os.open), not chmod'd afterwards — a later chmod leaves a
    # window in which the secrets are readable by anyone on the box.
    import stat

    path = tmp_path / ".env"
    sink = EnvFileSink(str(path))
    await sink.record("SECRET_TOKEN", "glpat-xyz")
    mode = stat.S_IMODE(path.stat().st_mode)
    assert mode == 0o600, f"expected 0600, got {mode:o}"


# ── PrintSink ──


async def test_print_sink_existing_reads_environ_only(monkeypatch):
    monkeypatch.setenv("PRINT_SET", "from-environ")
    monkeypatch.delenv("PRINT_UNSET", raising=False)
    sink = PrintSink()
    assert sink.existing("PRINT_SET") == "from-environ"
    assert sink.existing("PRINT_UNSET") == ""
    # Recording does not change what existing() sees (no file, no env write).
    await sink.record("PRINT_UNSET", "minted")
    assert sink.existing("PRINT_UNSET") == ""
    assert "PRINT_UNSET" not in os.environ


async def test_print_sink_flush_prints_export_lines_in_order(capsys):
    sink = PrintSink()
    await sink.record("VAR_ONE", "tok-1")
    await sink.record("VAR_TWO", "tok-2")
    await sink.flush()
    assert capsys.readouterr().out == "export VAR_ONE=tok-1\nexport VAR_TWO=tok-2\n"


async def test_print_sink_flush_without_records_prints_nothing(capsys):
    await PrintSink().flush()
    assert capsys.readouterr().out == ""


# ── SecretStoreSink ──


class _FakeStore:
    """Stands in for :class:`SecretValueStore` — plaintext, in memory."""

    def __init__(self, initial: dict[str, str] | None = None) -> None:
        self.values = dict(initial or {})
        self.puts: list[tuple[str, str, str, str]] = []

    async def load_all(self) -> dict[str, str]:
        return dict(self.values)

    async def put(self, name, value, *, updated_by, source) -> None:
        self.values[name] = value
        self.puts.append((name, value, updated_by, source))


async def test_secret_store_sink_records_write_through():
    store = _FakeStore()
    sink = SecretStoreSink(store, updated_by="cli:test", source="gitlab-provision")
    await sink.prime()
    await sink.record("GITLAB_TOKEN_SWE", "glpat-minted")
    # Committed inside record(), before any flush — a minted PAT is
    # unretrievable from GitLab, so it must not wait on a terminal flush.
    assert store.values["GITLAB_TOKEN_SWE"] == "glpat-minted"
    assert store.puts == [
        ("GITLAB_TOKEN_SWE", "glpat-minted", "cli:test", "gitlab-provision")
    ]
    await sink.flush()  # no-op safety net
    assert len(store.puts) == 1


async def test_secret_store_sink_existing_prefers_store_then_env(monkeypatch):
    monkeypatch.setenv("FROM_ENV_ONLY", "env-value")
    monkeypatch.setenv("IN_BOTH", "stale-env")
    monkeypatch.delenv("NOWHERE", raising=False)
    sink = SecretStoreSink(
        _FakeStore({"IN_BOTH": "stored", "STORE_ONLY": "s"}),
        updated_by="cli",
        source="cli",
    )
    await sink.prime()
    assert sink.existing("STORE_ONLY") == "s"
    # Store wins — the same precedence the engine resolver uses.
    assert sink.existing("IN_BOTH") == "stored"
    # Env is the fallback, so a seat already provisioned into the shell
    # still counts as provisioned and is never re-minted.
    assert sink.existing("FROM_ENV_ONLY") == "env-value"
    assert sink.existing("NOWHERE") == ""


async def test_secret_store_sink_recorded_value_is_immediately_visible():
    # Re-run idempotency within a single run: a var minted earlier in the
    # loop must not be minted a second time.
    sink = SecretStoreSink(_FakeStore(), updated_by="cli", source="cli")
    await sink.prime()
    assert sink.existing("TOKEN") == ""
    await sink.record("TOKEN", "v1")
    assert sink.existing("TOKEN") == "v1"
