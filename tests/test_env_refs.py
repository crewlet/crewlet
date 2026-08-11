"""The shared ``${VAR}`` grammar.

These pin the property that matters: every module deciding "is this a
reference?" must decide it the SAME way the resolver does. The two
divergences this module replaced were both real bugs — see
``crewlet.env_refs``.
"""

from __future__ import annotations

import re

from crewlet.env_refs import (
    ENV_VAR_PATTERN,
    env_var_reference,
    has_env_reference,
    is_env_var_name,
    referenced_env_vars,
)

# ── the grammar itself ──


def test_matches_valid_identifier_references():
    assert ENV_VAR_PATTERN.findall("${TOKEN}") == ["TOKEN"]
    assert ENV_VAR_PATTERN.findall("Bearer ${T_1}") == ["T_1"]
    assert ENV_VAR_PATTERN.findall("${_LEADING}") == ["_LEADING"]
    assert ENV_VAR_PATTERN.findall("${A}${B}") == ["A", "B"]


def test_ignores_shell_parameter_expansion():
    # The whole reason the grammar is identifier-only: substituting these
    # would mangle config-authored script content (a sandbox setup step's
    # helper script), usually to "".
    for text in ("${line#host=}", "${1:-x}", "${A.B}", "${}", "$BARE", "${no-dash}"):
        assert ENV_VAR_PATTERN.findall(text) == [], text


def test_rejects_leading_digit():
    # The provisioning sink used ``\w+``, which accepts ${1} — a name the
    # resolver would never substitute, so a credential minted into it
    # would be write-only.
    assert ENV_VAR_PATTERN.findall("${1}") == []
    assert not is_env_var_name("1ABC")
    assert is_env_var_name("ABC1")
    assert is_env_var_name("_x")


# ── has_env_reference ──


def test_has_env_reference_means_substitution_would_change_the_value():
    assert has_env_reference("${TOKEN}")
    assert has_env_reference("Bearer ${TOKEN}")
    assert not has_env_reference("literal")
    # The redaction leak: a literal secret carrying brace syntax the
    # resolver ignores is NOT a reference, so it must still be masked.
    assert not has_env_reference("pat-${line#host=}-literal")
    assert not has_env_reference(42)
    assert not has_env_reference(None)


# ── env_var_reference ──


def test_env_var_reference_accepts_exactly_one_whole_reference():
    assert env_var_reference("${PLANE_WEBHOOK_SECRET}") == "PLANE_WEBHOOK_SECRET"
    assert env_var_reference("  ${TOKEN}  ") == "TOKEN"


def test_env_var_reference_rejects_literal_embedded_and_multi():
    for value in ("", "whsec_literal", "wh-${SECRET}", "${SECRET}-suffix", "${A}${B}"):
        assert env_var_reference(value) == "", value


# ── referenced_env_vars ──


def test_referenced_env_vars_first_seen_order_and_dedupe():
    values = ["${B}", "x ${A} y ${B}", "${A}", "literal", 42, None]
    assert referenced_env_vars(values) == ["B", "A"]


def test_referenced_env_vars_empty():
    assert referenced_env_vars([]) == []
    assert referenced_env_vars(["literal"]) == []


# ── the anti-divergence guard ──


def test_config_reexports_the_same_pattern_object():
    """``config._ENV_VAR_PATTERN`` must BE the shared one, not a copy.

    A re-spelled copy is how the four grammars drifted apart in the first
    place; identity here makes a future divergence impossible rather than
    merely unlikely.
    """
    from crewlet.config import _ENV_VAR_PATTERN

    assert _ENV_VAR_PATTERN is ENV_VAR_PATTERN


#: Modules allowed to compile their own ``${...}`` regex, with the reason.
#: Everything else must import the shared grammar — four separate copies
#: is how the redaction leak and the ``${1}`` minting hole both happened.
_GRAMMAR_EXEMPT = {
    # The definition itself.
    "env_refs.py": "owns the grammar",
    # A DIFFERENT namespace that merely shares identifier syntax: skill
    # bodies substitute operator-defined `skill_variables`, never process
    # environment. Its key authority is SKILL_VARIABLE_KEY_PATTERN in the
    # same module, and coupling it to the env grammar would mean a future
    # change to env-var syntax silently redefined skill variables too.
    "agent/skills/models.py": "skill_variables namespace, not env vars",
}


def test_no_module_respells_the_env_var_grammar():
    """No module may compile its own ``${VAR}`` regex.

    Guards the invariant structurally: a new ``re.compile(r"\\$\\{...")``
    under ``src/crewlet`` fails here rather than surfacing months later as
    a redaction leak or an unreadable minted credential.
    """
    from pathlib import Path

    root = Path(__file__).resolve().parents[1] / "src" / "crewlet"
    pattern = re.compile(r"re\.compile\(\s*r?[\"'][^\"']*\\\$\\\{")
    offenders = sorted(
        str(path.relative_to(root))
        for path in root.rglob("*.py")
        if str(path.relative_to(root)) not in _GRAMMAR_EXEMPT
        and pattern.search(path.read_text())
    )
    assert offenders == [], (
        f"these modules compile their own ${{VAR}} regex instead of using "
        f"crewlet.env_refs: {offenders}"
    )
