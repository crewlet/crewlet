"""The ``${VAR}`` reference contract — ONE grammar, shared by every reader.

A config value may *point at* a secret instead of carrying it:
``"${SLACK_BOT_TOKEN_CEO}"`` is a reference, resolved to a real value at
construction time.  Four independent parts of the codebase have to agree
on what a reference looks like:

- :mod:`crewlet.config` **substitutes** them (the resolver),
- :mod:`crewlet.secrets.registry` **skips** them when masking secrets for
  display — a reference is an inert pointer, not a secret at rest,
- :mod:`crewlet.provisioning` **mints credentials into** them (the
  config's own references are the provisioning contract),
- :class:`crewlet.org.models.Role` decides whether a contact identity is
  a reference or a literal.

Each used to carry its own regex, and they disagreed.  Divergence here is
never a style question — it is a correctness or security bug:

- ``\\$\\{[^}]+\\}`` (the redaction registry) matched braces the resolver
  ignores, so a literal secret containing ``${line#host=}`` counted as "a
  reference" and was displayed **unmasked**.
- ``\\$\\{(\\w+)\\}`` (the provisioner) accepted ``${1}``, a name the
  resolver would never substitute — so a provisioner could mint a
  credential into a var nothing will ever read.

So the grammar lives here, once.  This module deliberately imports
nothing from ``crewlet``, which is what lets *every* one of those modules
depend on it — including the ones :mod:`crewlet.config` itself imports.

Not to be confused with :mod:`crewlet._env`, which decides which ``.env``
*file* a CLI loads.  This module is about the reference syntax inside
config values; that one is about where the process environment comes from.
"""

from __future__ import annotations

import re
from collections.abc import Iterable

#: Valid environment-variable NAMES only — the same identifier rule the
#: shell uses.  Deliberately does NOT match shell parameter expansions
#: (``${1:-x}``, ``${line#host=}``) or unbraced ``$VAR``: substituting
#: those would silently mangle config-authored script content, such as a
#: sandbox setup step's helper script.
ENV_VAR_PATTERN = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")

#: Anchored form of :data:`ENV_VAR_PATTERN` — the value is EXACTLY one
#: whole reference and nothing else.
_WHOLE_REF = re.compile(r"^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$")


def has_env_reference(value: object) -> bool:
    """True when ``value`` is a string carrying at least one ``${VAR}``.

    "Carries a reference" means "substitution would actually change it" —
    which is exactly the property display-redaction keys off when it
    decides a value is a pointer rather than a secret at rest.
    """
    return isinstance(value, str) and bool(ENV_VAR_PATTERN.search(value))


def env_var_reference(value: str) -> str:
    """The var name when ``value`` is EXACTLY one whole ``${VAR}``.

    Returns ``""`` for a literal, an embedded reference
    (``"prefix-${VAR}"``) or a multi-reference value (``"${A}${B}"``) —
    the shapes a capture-into-one-var contract can never satisfy, because
    the resolved value would be a concatenation rather than the captured
    secret itself.  Surrounding whitespace is tolerated.
    """
    if not isinstance(value, str):
        return ""
    match = _WHOLE_REF.match(value.strip())
    return match.group(1) if match else ""


def referenced_env_vars(values: Iterable[object]) -> list[str]:
    """Every ``${VAR}`` name referenced across ``values``, first-seen order.

    Non-string items are skipped.  Order is stable and duplicates collapse
    to their first position, so a provisioner reporting "these vars need
    minting" reads the same way on every run.
    """
    seen: dict[str, None] = {}
    for value in values:
        if isinstance(value, str):
            for match in ENV_VAR_PATTERN.finditer(value):
                seen.setdefault(match.group(1), None)
    return list(seen)


def is_env_var_name(name: str) -> bool:
    """True when ``name`` is a variable name a ``${...}`` reference can hold.

    A name failing this can never be reached by config resolution, so
    storing a secret under it would be write-only.
    """
    return bool(re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name))


__all__ = [
    "ENV_VAR_PATTERN",
    "env_var_reference",
    "has_env_reference",
    "is_env_var_name",
    "referenced_env_vars",
]
