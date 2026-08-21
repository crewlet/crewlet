"""``scripts/mattermost-dev-bootstrap.sh`` — the parts CI can actually run.

The script needs a live Mattermost, so its orchestration is not testable
here. Its *helpers* are pure, and they are where the failures were: a
header parse that silently matched nothing on the awk every Debian box
ships, a token-existence check that read "I could not tell" as "there is
none", and credentials handed to child processes on a command line every
account on the host can read.

Two kinds of check, both honest for a shell script:

- the pure helpers are **executed**, by extracting their definitions and
  sourcing them into a real bash;
- the invariants that need a server are **asserted against the source**,
  the way ``tests/test_examples/test_local_stack.py`` pins the Plane
  bootstrap's.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "mattermost-dev-bootstrap.sh"

pytestmark = pytest.mark.skipif(
    shutil.which("bash") is None, reason="needs bash to exercise the helpers"
)


def _source_text() -> str:
    return SCRIPT.read_text(encoding="utf-8")


def _code() -> str:
    """The script with comment lines removed.

    Every assertion below is about what the script *runs*; the comments
    quote the very patterns being banned, in order to explain why.
    """
    return "\n".join(
        line
        for line in _source_text().splitlines()
        if not line.lstrip().startswith("#")
    )


def _helpers() -> str:
    """The script's top-level function definitions, and nothing else."""
    text = _code()
    blocks = re.findall(r"^[a-z_]+\(\) \{\n.*?^\}$", text, re.MULTILINE | re.DOTALL)
    assert blocks, "no helper definitions found — did the script's shape change?"
    return "\n\n".join(blocks)


def _run(body: str, *, stdin: str = "") -> subprocess.CompletedProcess[str]:
    script = f"set -uo pipefail\n{_helpers()}\n{body}"
    return subprocess.run(
        ["bash", "-c", script],
        input=stdin,
        capture_output=True,
        text=True,
        timeout=60,
    )


# --- json_get -------------------------------------------------------------


@pytest.mark.parametrize(
    ("document", "path", "expected"),
    [
        ('{"id":"abc"}', "id", "abc"),
        ('{"a":{"b":"c"}}', "a.b", "c"),
        ('{"ServiceSettings":{"SiteURL":true}}', "ServiceSettings.SiteURL", "true"),
        ('{"id":"abc"}', "missing", ""),
        ("not json at all", "id", ""),
        ("[]", "id", ""),
    ],
)
def test_json_get_reads_the_document_from_stdin(document, path, expected):
    result = _run(f"json_get {path}", stdin=document)
    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == expected


# --- has_token ------------------------------------------------------------


def _token_list(count: int, *, described: str | None = None) -> str:
    tokens = [{"description": "other", "id": f"t{i}"} for i in range(count)]
    if described and tokens:
        tokens[0]["description"] = described
    return json.dumps(tokens)


@pytest.mark.parametrize(
    ("payload", "page_size", "status"),
    [
        (_token_list(3, described="crewlet-dev-bootstrap"), 200, 0),
        (_token_list(3), 200, 1),
        ("[]", 200, 1),
        # A FULL page is not proof of absence — the match may be on the
        # next one, and the caller mints an unrecoverable PAT on "absent".
        (_token_list(5), 5, 2),
        (_token_list(5, described="crewlet-dev-bootstrap"), 5, 0),
        ('{"message":"denied"}', 200, 2),
        ("garbage", 200, 2),
    ],
)
def test_has_token_is_three_valued(payload, page_size, status):
    result = _run(f"has_token crewlet-dev-bootstrap {page_size}", stdin=payload)
    assert result.returncode == status, result.stderr


def test_has_token_does_not_match_a_longer_description():
    """A substring match would read a `-old` token as the current one."""
    payload = json.dumps([{"description": "crewlet-dev-bootstrap-old"}])
    assert _run("has_token crewlet-dev-bootstrap 200", stdin=payload).returncode == 1


# --- env_get --------------------------------------------------------------


def test_env_get_understands_export_and_quotes(tmp_path):
    env = tmp_path / ".env"
    env.write_text(
        "export QUOTED=\"tok-123\"\n# a comment\nPLAIN=http://x:8065\nSQ='sq'\n"
    )
    for key, expected in (
        ("QUOTED", "tok-123"),
        ("PLAIN", "http://x:8065"),
        ("SQ", "sq"),
        ("ABSENT", ""),
    ):
        result = _run(f'env_get "{env}" {key}')
        assert result.returncode == 0, result.stderr
        assert result.stdout.strip() == expected


# --- the session-token header parse ---------------------------------------


def test_the_login_header_parse_is_case_insensitive_on_any_awk(tmp_path):
    """Go canonicalises the response header to ``Token:``. ``IGNORECASE``
    is a gawk extension — mawk (Debian, Ubuntu) and BSD awk (macOS) treat
    it as an ordinary variable, so a case-sensitive match found nothing
    and every run died at "Login failed" against a healthy server."""
    headers = tmp_path / "headers"
    headers.write_text("Content-Type: application/json\r\nToken: abc123def\r\n")

    parse = re.search(
        r"^SESSION=\$\((.*?)\)\n", _source_text(), re.MULTILINE | re.DOTALL
    )
    assert parse, "the session-header parse moved — update this test"
    body = parse.group(1).replace('"$headers"', f'"{headers}"')

    result = _run(f'printf "%s" "$({body})"')
    assert result.returncode == 0, result.stderr
    assert result.stdout == "abc123def"


def test_no_gawk_only_extensions():
    assert "IGNORECASE" not in _code()


# --- credentials never reach a command line -------------------------------


def test_every_authenticated_call_goes_through_the_stdin_helper():
    """``/proc/<pid>/cmdline`` is world-readable. The only Authorization
    header in the file belongs to api(), which pipes it to ``curl -K -``."""
    text = _code()
    header_lines = [line for line in text.splitlines() if "Authorization" in line]
    assert len(header_lines) == 1, header_lines
    assert "curl -K -" in header_lines[0]


def test_password_bearing_bodies_are_piped_not_argued():
    text = _code()
    for body in ("create_body", "login_body"):
        assert f'-d "${body}"' not in text, f"{body} is passed on argv"
        assert f"printf '%s' \"${body}\"" in text


def test_the_env_writer_takes_its_token_from_the_environment():
    text = _code()
    assert 'python3 - "$ENV_FILE" "$URL" "$ADMIN_TOKEN"' not in text
    assert 'MM_ADMIN_TOKEN="$ADMIN_TOKEN"' in text


def test_the_temp_env_file_is_created_owner_only():
    """``write_text`` then ``chmod`` leaves the PAT world-readable in
    between — and permanently, at 0644, if the run dies there."""
    text = _code()
    assert "tmp.write_text(" not in text
    assert "os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600" in text


def test_the_script_parses():
    assert subprocess.run(["bash", "-n", str(SCRIPT)]).returncode == 0
