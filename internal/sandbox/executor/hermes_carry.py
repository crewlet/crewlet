# A Hermes Agent 0.23.3 session as text, and back, and the run's managed scope (internal/sandbox/executor's
# hermescarry.go).
#
#   hermes_carry.py dump DB TITLE POLICY OUT        the session titled TITLE, with its compaction lineage,
#                                                   written to OUT
#   hermes_carry.py renew DB TITLE SYSTEM           SYSTEM's text as the standing instructions of the session
#                                                   titled TITLE, in place of the ones Hermes froze with it
#   hermes_carry.py restore MEMBER DB POLICY TITLE  a new session store at DB from MEMBER, the launch kept at
#                                                   POLICY
#   hermes_carry.py overlay STAGED MANAGED HOSTS    the run's managed scope at MANAGED: the box's own with
#                                                   STAGED's keys over it, and HOSTS (comma-separated) direct
#   hermes_carry.py top DB                          the store's newest message id (0 for none), where a
#                                                   launch's own rows begin
#   hermes_carry.py route DB TITLE MODEL            whether the box's model route (provider, base URL, model)
#                                                   is the one Hermes froze with the session titled TITLE
#   hermes_carry.py guard PLUGIN SERVERS            whether the box's Hermes would start with the run's plugin
#                                                   and its servers (JSON: each name's own header names) as the
#                                                   run wrote them
#
# Run under Hermes's own interpreter through its launcher (`hermes --run-module cProfile -o /dev/null`),
# which keeps no exit status, so every command ends on one line of its own on stdout: `crewlet-carry ok`
# (with the restored session's stored id after a restore, the id after top), or `crewlet-carry none` where
# the store holds no session of that title (or, for renew, no launch Hermes froze with it; for route, no
# route), and route answers `crewlet-carry moved` for another route, guard `crewlet-carry refused <why>`
# where the box would switch the run's own plugin or servers off. Anything else is a failure, its traceback
# on stderr.
#
# A dump is JSON lines: the store's schema ({"ddl", "kind"}, never the full-text index's own tables, which
# its triggers fill again), the store's own bookkeeping rows whole, then the session's rows ({"t", "r"}),
# and the launch Hermes froze with it ({"crewlet_policy"}). Text, so the engine's scrub can rewrite it.
# Standard library only, but for overlay, route and guard, which read the box's configuration through
# Hermes's own modules.
import base64
import copy
import json
import os
import re
import sqlite3
import sys
from pathlib import Path

# The store's own bookkeeping, whole but for state_meta's keys of one session or one admission, carried
# only for the dumped lineage's own.
WHOLE = ("schema_version", "state_meta", "runtime_epoch")
POLICY_KEY = "gateway.local_policy.v1:%s"
ADMISSION_KEY = "gateway.admission.result.v1.%s"
PER_SESSION = ("gateway.local_policy.v1:", "gateway.admission.result.v1.")
SESSION_ROWS = (("messages", "session_id"), ("session_model_usage", "session_id"),
                ("session_admissions", "target_session_id"))
INDEX_SUFFIXES = ("_config", "_data", "_docsize", "_idx", "_content")


def enc(v):
    return {"$b64": base64.b64encode(v).decode()} if isinstance(v, bytes) else v


def dec(v):
    return base64.b64decode(v["$b64"]) if isinstance(v, dict) and "$b64" in v else v


def continues(parent_end, source, model_config, parent):
    """Whether a child session is its parent's compaction continuation, as 0.23.3's
    lineage.is_compression_child reads it: the parent ended "compression", the child is no tool's, and no
    branch, delegation or reset marker names the parent. A delegated subagent's session names its parent
    too, and is not the conversation the run's notes and transcript come from."""
    try:
        config = json.loads(model_config) if model_config else {}
    except ValueError:
        config = {}
    config = config if isinstance(config, dict) else {}
    markers = {config.get(k) for k in ("_branched_from", "_delegate_from", "_reset_from")}
    return parent_end == "compression" and source != "tool" and parent not in markers


def lineage(db, title):
    """The session titled title with every session a compaction continued it from or in, oldest first. A box
    whose compression.in_place is false has Hermes rotate a session on compaction (measured on 0.23.3): the
    title moves to the child, so the walk starts there and goes back through the sessions it continues."""
    row = db.execute("select id from sessions where title = ? order by started_at limit 1", (title,)).fetchone()
    if row is None:
        return []
    root, seen = row[0], {row[0]}
    while True:
        child = db.execute("select parent_session_id, source, model_config from sessions where id = ?", (root,)).fetchone()
        parent = child[0] if child else None
        end = db.execute("select end_reason from sessions where id = ?", (parent,)).fetchone() if parent else None
        if end is None or parent in seen or not continues(end[0], child[1], child[2], parent):
            break
        root = parent
        seen.add(root)
    ids = [root]
    for sid in ids:
        end = db.execute("select end_reason from sessions where id = ?", (sid,)).fetchone()[0]
        ids.extend(r[0] for r in db.execute("select id, source, model_config from sessions where parent_session_id = ? "
                                            "order by started_at", (sid,))
                   if r[0] not in ids and continues(end, r[1], r[2], sid))
    return ids


def dump(db_path, title, policy_path, out_path):
    db = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)
    db.row_factory = sqlite3.Row
    # ONE SNAPSHOT: a live gateway may write between the reads, and a torn dump restores a turn half settled.
    db.execute("begin")
    ids = lineage(db, title)
    if not ids:
        return "none"
    out = open(out_path, "w", encoding="utf-8")
    put = lambda o: out.write(json.dumps(o, ensure_ascii=False) + "\n")
    virtual = {r[0] for r in db.execute("select name from sqlite_master where type = 'table' and sql like 'CREATE VIRTUAL TABLE%'")}
    for kind, name, sql in db.execute("select type, name, sql from sqlite_master where sql is not null order by "
                                      "case type when 'table' then 0 when 'index' then 1 else 2 end, rowid"):
        if name == "sqlite_sequence" or (kind == "table" and any(name == v + s for v in virtual for s in INDEX_SUFFIXES)):
            continue
        put({"ddl": sql, "kind": kind})
    rows = lambda q, a=(): [dict(r) for r in db.execute(q, a)]
    marks = ["?"] * len(ids)
    admitted = [r[0] for r in db.execute('select admission_id from session_admissions where target_session_id in (%s)'
                                         % ",".join(marks), ids)]
    own = {POLICY_KEY % sid for sid in ids} | {ADMISSION_KEY % a for a in admitted}
    for table in WHOLE:
        for r in rows('select * from "%s"' % table):
            if table != "state_meta" or not r["key"].startswith(PER_SESSION) or r["key"] in own:
                put({"t": table, "r": {k: enc(v) for k, v in r.items()}})
    for sid in ids:
        for r in rows("select * from sessions where id = ?", (sid,)):
            put({"t": "sessions", "r": {k: enc(v) for k, v in r.items()}})
        for table, column in SESSION_ROWS:
            for r in rows('select * from "%s" where "%s" = ? order by rowid' % (table, column), (sid,)):
                put({"t": table, "r": {k: enc(v) for k, v in r.items()}})
        for r in rows("select p.* from system_prompts p join sessions s on p.hash = s.system_prompt_hash where s.id = ?", (sid,)):
            put({"t": "system_prompts", "r": {k: enc(v) for k, v in r.items()}})
    if os.path.isfile(policy_path):
        with open(policy_path, encoding="utf-8") as f:
            put({"crewlet_policy": f.read()})
    out.close()
    return "ok"


def restore(member, db_path, policy_path, title):
    with open(member, encoding="utf-8") as f:
        lines = [json.loads(line) for line in f if line.strip()]
    if os.path.exists(db_path):
        os.remove(db_path)
    db = sqlite3.connect(db_path)
    for line in lines:
        if "ddl" in line and line.get("kind") != "trigger":
            db.execute(line["ddl"])
    for line in lines:
        if line.get("kind") == "trigger":
            db.execute(line["ddl"])
    policy, stored = None, None
    for line in lines:
        if "t" in line:
            if line["t"] == "sessions" and line["r"].get("title") == title and stored is None:
                stored = line["r"].get("id")
            cols = list(line["r"])
            db.execute('insert or replace into "%s" (%s) values (%s)' % (
                line["t"], ",".join('"%s"' % c for c in cols), ",".join("?" * len(cols))),
                [dec(line["r"][c]) for c in cols])
        elif "crewlet_policy" in line:
            policy = line["crewlet_policy"]
    db.commit()
    db.close()
    if policy is not None:
        with open(policy_path, "w", encoding="utf-8") as f:
            f.write(policy)
    if not stored:
        return "none"
    return "ok " + stored


SYSTEM_ENV = "HERMES_EPHEMERAL_SYSTEM_PROMPT"


def renew(db_path, title, system_path):
    """Hermes freezes a session's client environment, the standing instructions among it, in its local
    policy (state_meta) as it creates the session, and a resume keeps it; the run's own are written there
    before the resume, with the gateway stopped."""
    try:
        with open(system_path, encoding="utf-8") as f:
            # As the launch's $(cat) reads it, trailing newlines dropped.
            system = f.read().rstrip("\n")
    except FileNotFoundError:
        system = ""
    db = sqlite3.connect(db_path)
    renewed = 0
    for sid in lineage(db, title):
        row = db.execute("select value from state_meta where key = ?", (POLICY_KEY % sid,)).fetchone()
        if row is None:
            continue
        local = json.loads(row[0])
        policy = local.get("policy") or {}
        env = json.loads(policy.get("client_env_json") or "{}")
        if system:
            env[SYSTEM_ENV] = system
        else:
            env.pop(SYSTEM_ENV, None)
        # Written back as Hermes writes both (measured on 0.23.3): the environment spaced with non-ASCII
        # escaped, the whole compact with it raw, each with its keys sorted.
        policy["client_env_json"] = json.dumps(env, sort_keys=True)
        local["policy"] = policy
        db.execute("update state_meta set value = ? where key = ?",
                   (json.dumps(local, sort_keys=True, separators=(",", ":"), ensure_ascii=False), POLICY_KEY % sid))
        renewed += 1
    db.commit()
    return "ok" if renewed else "none"


def top(db_path):
    """The newest message id in the store: a launch's own rows are those above it."""
    if not os.path.isfile(db_path):
        return "ok 0"
    db = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)
    if db.execute("select 1 from sqlite_master where type = 'table' and name = 'messages'").fetchone() is None:
        return "ok 0"
    return "ok %d" % (db.execute("select coalesce(max(id), 0) from messages").fetchone()[0])


def configured_model(model):
    """A model section's default model; a bare string names it alone, as Hermes reads it."""
    return (model.get("default") if isinstance(model, dict) else model) or ""


def route(db_path, title, model):
    """Hermes keeps a session on the route it froze as it created it (its gateway_runtime's provider and base
    URL, written before the first request answers), whatever the box's configuration says at a resume
    (measured on 0.23.3: a resume under another provider and base URL still calls the frozen one), so a
    continued session would keep a route its entry no longer names. A resume is right only on the route Hermes
    resolves for this launch now, as session.create resolved the frozen one: -m's words through
    resolve_launch_route (anthropic:m names anthropic whatever the box configures), then
    hermes_cli.runtime_provider (measured equal to the frozen route for -m anthropic:m on a box configured for
    openrouter, for a named provider and for one found from a key). The model: a run naming one has it
    compared with its kept launch, since -m's words are not the frozen form (openrouter:m freezes m); a run
    naming none, the configured default against the frozen configuration's, each over DEFAULT_CONFIG as
    Hermes's loader reads them (a gateway worker reads its frozen configuration so). moved where any differs,
    none where no route was frozen. The box's own managed scope is read; the run's overlay sets no model key."""
    os.environ.pop("HERMES_MANAGED_DIR", None)
    db = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)
    ids = lineage(db, title)
    row = db.execute("select model_config from sessions where id = ?", (ids[-1],)).fetchone() if ids else None
    frozen = (json.loads(row[0] or "{}") or {}).get("gateway_runtime") if row else None
    if not isinstance(frozen, dict) or not frozen.get("provider"):
        return "none"
    from gateway.session_local_route import resolve_launch_route
    from hermes_cli.config import DEFAULT_CONFIG, _deep_merge, load_config
    from hermes_cli.runtime_provider import resolve_runtime_provider
    box = load_config()
    launch = resolve_launch_route({"model": model}, box) if model else {}
    now = resolve_runtime_provider(requested=launch.get("provider") or None, explicit_api_key=launch.get("api_key"),
                                   explicit_base_url=launch.get("base_url"), target_model=launch.get("model"))
    same = (now.get("provider") == frozen.get("provider") and
            (now.get("base_url") or "").rstrip("/") == (frozen.get("base_url") or "").rstrip("/"))
    if same and not model:
        policy = db.execute("select value from state_meta where key = ?", (POLICY_KEY % ids[0],)).fetchone()
        config = json.loads(json.loads(policy[0]).get("policy", {}).get("config_json") or "{}") if policy else {}
        config = _deep_merge(copy.deepcopy(DEFAULT_CONFIG), config)
        same = configured_model(box.get("model")) == configured_model(config.get("model"))
    return "ok" if same else "moved"


def merged(under, over):
    """over's keys over under's, per leaf as Hermes merges a managed scope: a mapping key by key, any
    other value (a list among them) whole."""
    out = dict(under)
    for k, v in over.items():
        out[k] = merged(out[k], v) if isinstance(v, dict) and isinstance(out.get(k), dict) else v
    return out


def put(path, data):
    """data at path, mode 0600 (the run's overlay holds the bridge's token), moved into place whole; no
    file where data is None."""
    if data is None:
        if os.path.lexists(path):
            os.remove(path)
        return
    with open(os.open(path + ".next", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "wb") as f:
        f.write(data)
    os.replace(path + ".next", path)


# NO_PROXY's entries as Hermes splits them (agent/proxy_bypass.no_proxy_entries), and the characters a host,
# a host:port, a CIDR or a wildcard entry is made of: anything else is no host, and could not be written to a
# .env unquoted.
NO_PROXY_SPLIT = re.compile(r"[\s,]+")
NO_PROXY_ENTRY = re.compile(r"^[A-Za-z0-9.*:/_%\[\]-]+$")
NO_PROXY_LINE = re.compile(rb"^\s*(export\s+)?(NO_PROXY|no_proxy)\s*=")


def no_proxy(home_dir, box_env, hosts):
    """The one NO_PROXY list both spellings hold for the run: the process's (both spellings), the home's .env's
    and the box's managed .env's, each in that order, then hosts, the run's own routes; duplicates dropped, and
    hosts moved after the person's entries where the wrapper had appended them already."""
    from agent.secret_scope import load_env_file
    own = [h for h in hosts.split(",") if h]
    layers = [os.environ, load_env_file(Path(home_dir, ".env"))] + ([load_env_file(box_env)] if box_env else [])
    entries = [e for env in layers for key in ("NO_PROXY", "no_proxy")
               for e in NO_PROXY_SPLIT.split(env.get(key) or "")]
    out = []
    for e in [e for e in entries if e not in own] + own:
        if e and NO_PROXY_ENTRY.match(e) and e not in out:
            out.append(e)
    return ",".join(out)


def overlay(staged_path, managed_dir, hosts):
    """The run's HERMES_MANAGED_DIR hides the box's own managed scope (Hermes's default, /etc/hermes: an
    administrator's config.yaml and .env), so MANAGED holds that scope, read through Hermes's loader as
    Hermes reads it, with STAGED's keys over its config.yaml. A list is one leaf, so a run that disables
    toolsets disables the box's effective list (its config.yaml under that scope) and its own. Hermes loads
    the managed .env last, over the home's .env and the process's environment (0.23.3's load_hermes_dotenv,
    measured: a home .env's NO_PROXY replaced the run's), so where hosts (comma-separated) names the run's own
    routes the managed .env states NO_PROXY and no_proxy, both the merged list ([no_proxy])."""
    os.environ.pop("HERMES_MANAGED_DIR", None)
    from hermes_cli import managed_scope
    from hermes_cli.config import load_config
    with open(staged_path, encoding="utf-8") as f:
        run = json.load(f)
    config = merged(managed_scope.load_managed_config(), run)
    deny = (run.get("agent") or {}).get("disabled_toolsets")
    if deny is not None:
        box = (load_config().get("agent") or {}).get("disabled_toolsets")
        box = box if isinstance(box, list) else []
        config["agent"]["disabled_toolsets"] = box + [t for t in deny if t not in box]
    box_dir = managed_scope.get_managed_dir()
    env = box_dir / ".env" if box_dir is not None else None
    data = env.read_bytes() if env is not None and env.is_file() else None
    if hosts:
        kept = b"".join(line for line in (data or b"").splitlines(keepends=True) if not NO_PROXY_LINE.match(line))
        merged_list = no_proxy(os.environ.get("HERMES_HOME", ""), env if data is not None else None, hosts)
        data = (kept if not kept or kept.endswith(b"\n") else kept + b"\n") + \
            ("NO_PROXY=%s\nno_proxy=%s\n" % (merged_list, merged_list)).encode()
    os.makedirs(managed_dir, exist_ok=True)
    put(os.path.join(managed_dir, ".env"), data)
    put(os.path.join(managed_dir, "config.yaml"), json.dumps(config).encode())
    return "ok"


MISSING = object()

# What each of Hermes's launch switches costs the run (0.23.3's gateway_chat._ENV_LAUNCH_FLAGS; measured).
LAUNCH_SWITCHES = {"safe_mode": "without its MCP servers and plugins",
                   "ignore_user_config": "without the run's settings and plugins"}


def guard(plugin, servers, skills):
    """Why the box's Hermes would start without the run's plugin or one of servers' tools, by what Hermes itself
    reads (load_config() under the run's managed scope, its own gates), each reason naming the file, key and
    entry: a sticky profile (main._apply_profile_override's read), a launch switch in an environment or .env, a
    disabled toolset naming the server, a cli toolset list leaving it out, a header beside the server's own, an
    entry Hermes skips (suspicious or a malformed cost table: leaves the run's overlay cannot clear), or the plugin
    gated off, or one of skills (the run's seat skills' names) disabled for the cli, by Hermes's own
    get_disabled_skills, which Hermes leaves unlisted (read in its code). The run's overlay
    states every leaf of its own servers it can (hermesconfig.go), so those are not read here."""
    names = json.loads(servers)
    os.environ.pop("CREWLET_HERMES_PLUGIN", None)  # a plugin Hermes loads while answering registers nothing
    run_scope = os.environ.get("HERMES_MANAGED_DIR", "")
    home_dir = os.environ.get("HERMES_HOME", "")
    from hermes_cli import managed_scope
    from hermes_cli.config import get_config_path, load_config, read_raw_config
    config = load_config()
    layers = [(str(get_config_path()), read_raw_config())]
    os.environ.pop("HERMES_MANAGED_DIR", None)
    box_dir = managed_scope.get_managed_dir()
    if box_dir is not None:
        layers.append((str(box_dir / "config.yaml"), managed_scope.load_managed_config()))
    os.environ["HERMES_MANAGED_DIR"] = run_scope
    found = []

    def where(path, holds, names_only=False):
        """Each file whose own value at path holds, as `file: its path is value`, joined; a mapping that may
        hold a credential (an env, headers) by its keys alone."""
        out = []
        for file, doc in layers:
            v = doc
            for k in path:
                v = v.get(k, MISSING) if isinstance(v, dict) else MISSING
            if v is not MISSING and holds(v):
                shown = "sets " + ", ".join(sorted(map(str, v))) if names_only and isinstance(v, dict) else \
                    "is " + json.dumps(v, default=str)
                out.append("%s: its %s %s" % (file, ".".join(path), shown))
        return " and ".join(out) or "Hermes's own %s" % ".".join(path)

    from pathlib import Path
    from hermes_constants import get_default_hermes_root
    if not (home_dir and Path(home_dir).parent.name == "profiles"):
        marker = get_default_hermes_root() / "active_profile"
        try:
            profile = marker.read_text(encoding="utf-8-sig").strip()
        except (OSError, UnicodeDecodeError):
            profile = ""
        if profile and profile != "default":
            found.append("%s names the profile %s, so Hermes would run in that profile's home, without the run's "
                         "plugin and settings (set it to default)" % (marker, json.dumps(profile)))

    from agent.secret_scope import load_env_file
    from hermes_cli.env_loader import load_hermes_dotenv
    from hermes_cli.gateway_chat import _ENV_LAUNCH_FLAGS, env_launch_flags
    from utils import is_truthy_value
    sources = [("the run's environment", dict(os.environ)),
               (os.path.join(home_dir, ".env"), load_env_file(Path(home_dir, ".env")))]
    if box_dir is not None:
        sources.append((str(box_dir / ".env"), load_env_file(box_dir / ".env")))
    load_hermes_dotenv(hermes_home=home_dir, load_external_secrets=False)
    on = env_launch_flags()
    for field, var in _ENV_LAUNCH_FLAGS:
        if field in LAUNCH_SWITCHES and on.get(field):
            held = ["%s: %s=%s" % (name, var, env[var]) for name, env in sources if is_truthy_value(env.get(var))]
            found.append("%s, which starts Hermes %s (remove it)" % (" and ".join(held) or "Hermes's environment: " + var,
                                                                  LAUNCH_SWITCHES[field]))

    from agent.skill_utils import parse_config_string_list
    from hermes_cli.tools_config import _get_platform_tools
    from tools.mcp_tool_config import _malformed_cost_table, _suspicious_mcp_issues

    def toolsets(value):
        return [n.strip() for n in parse_config_string_list(value) if n.strip()] if value else []

    entries = config.get("mcp_servers") if isinstance(config.get("mcp_servers"), dict) else {}
    denied = toolsets((config.get("agent") or {}).get("disabled_toolsets"))
    cli = _get_platform_tools(config, "cli")
    for name, sent in names.items():
        entry = entries.get(name)
        if not isinstance(entry, dict):
            continue
        headers = entry.get("headers")
        if isinstance(headers, dict) and any(k not in sent for k in headers):
            found.append("%s, which Hermes sends to the server %s beside the run's own headers (remove %s)" % (
                where(("mcp_servers", name, "headers"), lambda v: isinstance(v, dict) and any(k not in sent for k in v),
                      True), name, " and ".join(sorted(k for k in headers if k not in sent))))
        own = (name, "mcp-" + name)
        if any(d in own for d in denied):
            found.append("%s, which hides the server %s's tools (remove %s)" % (
                where(("agent", "disabled_toolsets"), lambda v: any(d in own for d in toolsets(v))), name,
                " and ".join(d for d in own if d in denied)))
        elif name not in cli:
            found.append("%s, which leaves the server %s out of Hermes's tools (add %s, or drop no_mcp)" % (
                where(("platform_toolsets", "cli"), lambda v: True), name, name))
        issues = _suspicious_mcp_issues(name, entry)
        if issues:
            found.append("%s, so Hermes skips the server %s: %s" % (
                where(("mcp_servers", name, "env"), lambda v: True, True), name, "; ".join(issues)))
        bad = _malformed_cost_table(name, entry)
        if bad is not None:
            found.append("%s, so Hermes skips the server %s: %s" % (
                where(("mcp_servers", name, "tool_costs"), lambda v: True), name, bad))

    from hermes_cli.skills_config import get_disabled_skills
    off = get_disabled_skills(config, "cli")
    for skill in json.loads(skills):
        if skill in off:
            said = [where(path, lambda v, s=skill: s in toolsets(v)) for path in (("skills", "disabled"),
                                                                                   ("skills", "platform_disabled", "cli"))]
            found.append("%s, which leaves the run's skill %s unlisted (remove it)" % (
                " and ".join(w for w in said if not w.startswith("Hermes's own")) or said[0], skill))

    from hermes_cli.plugins_discovery import (_get_disabled_plugins, _get_enabled_plugins, collect_directory_manifests,
                                              gate_manifest)
    from hermes_cli.plugins_manifest import manifest_key
    ours = Path(home_dir, "plugins", plugin).resolve()
    for m in collect_directory_manifests():
        if manifest_key(m) != plugin or not m.path or Path(m.path).resolve() != ours:
            continue
        disabled, enabled = _get_disabled_plugins(), _get_enabled_plugins()
        verdict = gate_manifest(m, disabled, enabled)
        if verdict.action not in ("load", "load_now", "defer"):
            held = (where(("plugins", "disabled"), lambda v: isinstance(v, list) and plugin in v) if plugin in disabled
                    else where(("plugins", "enabled"), lambda v: True))
            found.append("%s, so Hermes would not load the run's plugin %s: %s" % (held, plugin, verdict.error))
    return "ok" if not found else "refused " + " | ".join(found).replace("\n", " ")


COMMANDS = {"dump": (dump, 4), "renew": (renew, 3), "restore": (restore, 4), "overlay": (overlay, 3), "top": (top, 1),
            "route": (route, 3), "guard": (guard, 3)}

if __name__ == "__main__":
    command = COMMANDS.get(sys.argv[1] if len(sys.argv) > 1 else "")
    if command is None or len(sys.argv) != command[1] + 2:
        sys.stderr.write("usage: hermes_carry.py dump DB TITLE POLICY OUT | renew DB TITLE SYSTEM | "
                         "restore MEMBER DB POLICY TITLE | overlay STAGED MANAGED HOSTS | top DB | route DB TITLE MODEL | "
                         "guard PLUGIN SERVERS SKILLS\n")
        sys.exit(2)
    sys.stdout.write("crewlet-carry %s\n" % command[0](*sys.argv[2:]))
