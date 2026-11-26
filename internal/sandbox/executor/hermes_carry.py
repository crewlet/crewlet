# A Hermes Agent 0.23.3 session as text, and back, and the run's managed scope (internal/sandbox/executor's
# hermescarry.go).
#
#   hermes_carry.py dump DB TITLE POLICY OUT        the session titled TITLE, with its compaction lineage,
#                                                   written to OUT
#   hermes_carry.py trim SRC DST                    the dump at SRC as a resume of it loads it, written to DST:
#                                                   a session's inactive messages below its newest compaction
#                                                   left out, its settled admissions' results slimmed
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
#   hermes_carry.py servers STAGED                  whether the box's Hermes runs each MCP server of STAGED as
#                                                   STAGED writes it
#   hermes_carry.py guard PLUGIN SERVERS SKILLS     whether the box's Hermes would start with the run's plugin
#                                                   and its servers (JSON: each name's own header names) as the
#                                                   run wrote them, and its skills listed
#   hermes_carry.py gate PEEK OUT MODEL BOUND       what Hermes makes of each checkout PEEK's roots name at the
#                                                   window it resolves for MODEL, the note helper's gate, written
#                                                   to OUT (checkout_gate); `off <why>` where Hermes's hook bound is
#                                                   under BOUND s, `moved <why>` where a CHECKOUT_INTERNALS entry moved
#
# Run under Hermes's own interpreter through its launcher (`hermes --run-module cProfile -o /dev/null`),
# which keeps no exit status, so every command ends on one line of its own on stdout: `crewlet-carry ok`
# (with the restored session's stored id after a restore, the id after top), or `crewlet-carry none` where
# the store holds no session of that title (or, for renew, no launch Hermes froze with it; for route, no
# route; for trim, no row to leave out), route answers `crewlet-carry moved` for another route, servers
# `crewlet-carry taken` and why, and guard `crewlet-carry refused <why>` where the box would switch the run's
# own plugin or servers off. Anything else is a failure, its traceback on stderr. CHECKOUT_INTERNALS names what the
# script reads of Hermes's own code for checkout handing, CONTRACT_INTERNALS what the rest reads.
#
# A dump is JSON lines: the store's schema ({"ddl", "kind"}, never the full-text index's own tables, which
# its triggers fill again), the store's own bookkeeping rows whole, then the session's rows ({"t", "r"}),
# and the launch Hermes froze with it ({"crewlet_policy"}). Text, so the engine's scrub can rewrite it.
# Standard library only, but for overlay, route, servers and guard, which read the box's configuration through
# Hermes's own modules.
import base64
import fnmatch
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


# What 0.23.3's own retirement drops of a settled admission's stored result, its copies of the turn's
# transcript (persistence/sqlite/conversation/_store/retirement.py, _compact_result), leaving `messages` empty.
RESULT_TRANSCRIPT = ("messages", "last_reasoning", "tools", "_turn_user_row")


def stored_result(value):
    """An admission's stored result as 0.23.3's terminal.load_result reads it, without the envelope a row
    stores its closing reply once by ({"_stored": {"v": 1, "reply_ref": i}}, i a message of the result's,
    whose words final_response holds); None where it does not read so."""
    try:
        v = json.loads(value) if isinstance(value, str) else None
    except ValueError:
        return None
    if not isinstance(v, dict) or not isinstance(v.get("result"), dict):
        return None
    env, messages = v.pop("_stored", None), v["result"].get("messages")
    if env is None:
        return v
    if not isinstance(env, dict) or set(env) != {"v", "reply_ref"} or env["v"] != 1 or not isinstance(messages, list):
        return None
    ref = env["reply_ref"]
    if type(ref) is not int or not 0 <= ref < len(messages) or not isinstance(messages[ref], dict):
        return None
    return v


def slimmed(line, row):
    """line, a settled admission's stored result, slimmed as 0.23.3's retirement slims it and written back as
    the measured trim wrote it; line itself where its value does not read as a result, or the slim cannot be
    written as text (a lone surrogate the value escaped)."""
    v = stored_result(row["r"]["value"])
    if v is None:
        return line
    v["result"] = dict({k: x for k, x in v["result"].items() if k not in RESULT_TRANSCRIPT}, messages=[])
    row["r"]["value"] = json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    out = json.dumps(row, ensure_ascii=False) + "\n"
    try:
        out.encode("utf-8")
    except UnicodeEncodeError:
        return line
    return out


def trim(src, dst):
    """The dump at src as a resume of it loads it, into dst (measured on 0.23.3: a resume of the trimmed dump
    sent the requests a resume of the whole one did). Left out: each session's inactive message rows below its
    newest active compaction summary, the compaction's archive and the originals of the tail it wrote again,
    which a resume never loads; an inactive row above it, one an undo left, stays, so the restored ids go on
    where they did. Each settled admission's stored result is slimmed as Hermes's own retirement slims one;
    every other line is kept as it is. "none", dst unwritten, where no row is left out. Read a line at a time,
    twice, the summaries first, so the dump is never held whole."""
    newest, settled, inactive = {}, set(), []
    with open(src, encoding="utf-8", newline="") as f:
        for line in f:
            o = json.loads(line)
            r = o.get("r") or {}
            if o.get("t") == "messages":
                if r["active"] == 1 and r["_compressed_summary"] == 1:
                    newest[r["session_id"]] = max(newest.get(r["session_id"], 0), r["id"])
                elif r["active"] == 0:
                    inactive.append((r["session_id"], r["id"]))
            elif o.get("t") == "session_admissions" and r["status"] == "terminal":
                settled.add(ADMISSION_KEY % r["admission_id"])
    if not any(i < newest.get(sid, 0) for sid, i in inactive):
        return "none"
    with open(src, encoding="utf-8", newline="") as f, open(dst, "w", encoding="utf-8", newline="") as out:
        for line in f:
            o = json.loads(line)
            r = o.get("r") or {}
            if o.get("t") == "messages" and r["active"] == 0 and r["id"] < newest.get(r["session_id"], 0):
                continue
            out.write(slimmed(line, o) if o.get("t") == "state_meta" and r["key"] in settled else line)
    return "ok"


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


def tree(db, title):
    """The session titled title's lineage with every session descending from it by parent_session_id, whatever
    the kind: 0.23.3 writes a delegated subagent's turns to a child session of its delegator in the same store
    (tools/delegate_tool_child_run.py: source "subagent", _delegate_from naming the parent), and that child's
    own compaction continuations and delegates descend from it in turn."""
    ids = lineage(db, title)
    seen = set(ids)
    for sid in ids:
        for (child,) in db.execute("select id from sessions where parent_session_id = ? order by started_at", (sid,)):
            if child not in seen:
                seen.add(child)
                ids.append(child)
    return ids


def requests(db_path, title, after):
    """The model responses the store holds for the run's session tree ([tree]) past message id after: its
    assistant rows but the notes Hermes writes as one (an interrupt's, a failed turn's), which carry a display_kind
    or display_metadata no response carries (measured on 0.23.3: a capped run's summary row included). A request
    an end cuts off before its answer leaves no row (measured)."""
    if not os.path.isfile(db_path):
        return "ok 0"
    db = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)
    if db.execute("select 1 from sqlite_master where type = 'table' and name = 'messages'").fetchone() is None:
        return "ok 0"
    db.execute("begin")
    ids = tree(db, title)
    if not ids:
        return "ok 0"
    return "ok %d" % db.execute("select count(*) from messages where session_id in (%s) and id > ? and role = "
                                "'assistant' and display_kind is null and display_metadata is null"
                                % ",".join("?" * len(ids)), ids + [int(after)]).fetchone()[0]


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


# The leaves of Crewlet's own servers the guard judges (a switch off, a tool filter, a header beside the run's own),
# which `servers` therefore passes over on those servers.
GUARDED_LEAVES = ("enabled", "disabled", "tools", "headers")


def servers(staged_path, guarded):
    """Whether the box's Hermes runs each MCP server of the run's overlay at STAGED as the overlay writes it, read
    through Hermes's own MCP loader as the gateway reads it (its home's config.yaml, the run's managed scope, then
    every enabled plugin package's mcp.json): `ok`, else `taken` and a sentence naming the server and what
    declares it. A box's same-named entry is merged under the run's per leaf, so any key of its own changes the
    server; on GUARDED (a JSON list of Crewlet's own servers) the leaves the guard judges, GUARDED_LEAVES, are
    its to refuse. A plugin's same-named server loses to the run's entry, but a live endpoint it declares for that
    name replaces the entry's URL (measured on 0.23.3: the run's `crewlet` then connected to the plugin's server,
    never the bridge), so a plugin's server under one of the run's names is refused whatever it declares. Hermes
    names a tool `mcp__<server>__<tool>`, its server's name with each character outside [A-Za-z0-9_] made `_`, so
    only a server named exactly as the run's can name a tool of the bridge's (code-sandbox.md, "The tool
    bridge")."""
    with open(staged_path, encoding="utf-8") as f:
        run = json.load(f).get("mcp_servers") or {}
    own = set(json.loads(guarded) or [])

    def judged(name, entry):
        if name not in own or not isinstance(entry, dict):
            return entry
        return {k: v for k, v in entry.items() if k not in GUARDED_LEAVES}

    from hermes_cli import managed_scope
    from hermes_cli.config import get_config_path, read_user_config_raw
    from hermes_cli.plugins import get_plugin_manager
    from tools.mcp_tool_config import _load_mcp_config
    resolved = _load_mcp_config()
    owners = get_plugin_manager().get_portable_mcp_server_plugins()
    home = read_user_config_raw().get("mcp_servers") or {}
    os.environ.pop("HERMES_MANAGED_DIR", None)
    managed_scope.invalidate_managed_cache()
    system = managed_scope.load_managed_config().get("mcp_servers") or {}
    system_dir = managed_scope.get_managed_dir()
    for name in sorted(run):
        if name not in owners and judged(name, resolved.get(name)) == judged(name, run[name]):
            continue
        why = []
        if isinstance(home, dict) and name in home:
            why.append("%s declares it" % get_config_path())
        if isinstance(system, dict) and name in system and system_dir is not None:
            why.append("%s declares it" % (system_dir / "config.yaml"))
        if name in owners:
            why.append("the plugin %s declares it" % json.dumps(owners[name]))
        if name not in resolved:
            why.append("Hermes runs no server of that name")
        return "taken the box's Hermes Agent does not run the run's MCP server %s as the run's settings write it " \
               "(%s); remove that from the box image" % (json.dumps(name), "; ".join(why) or "its configuration changes it")
    return "ok"


MISSING = object()

# What each of Hermes's launch switches costs the run (0.23.3's gateway_chat._ENV_LAUNCH_FLAGS; measured).
LAUNCH_SWITCHES = {"safe_mode": "without its MCP servers and plugins",
                   "ignore_user_config": "without the run's settings and plugins"}


def unreadable(path):
    """Why the box's configuration file at path cannot be read whole, or None: there but no regular file, not
    readable, or not YAML (where Hermes's yaml is at hand), which Hermes would load as empty, without a word."""
    if not os.path.lexists(path):
        return None
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except (OSError, UnicodeDecodeError) as e:
        return "%s is there but cannot be read (%s), so Hermes would run without it (fix or remove it)" % (path, e)
    try:
        import yaml
    except ImportError:
        return None
    try:
        yaml.safe_load(text)
    except yaml.YAMLError as e:
        return "%s does not read as YAML (%s), so Hermes would run without it (fix it)" % (path, str(e).replace("\n", " "))
    return None


def box_layers():
    """The box's own configuration files, the home's and the machine's managed one, each with its raw
    content, read as Hermes reads them, without the run's managed scope; and the managed directory."""
    from hermes_cli import managed_scope
    from hermes_cli.config import get_config_path, read_raw_config
    run_scope = os.environ.get("HERMES_MANAGED_DIR", "")
    layers = [(str(get_config_path()), read_raw_config())]
    os.environ.pop("HERMES_MANAGED_DIR", None)
    box_dir = managed_scope.get_managed_dir()
    if box_dir is not None:
        layers.append((str(box_dir / "config.yaml"), managed_scope.load_managed_config()))
    os.environ["HERMES_MANAGED_DIR"] = run_scope
    return layers, box_dir


def plugoff(plugin):
    """Whether a file of the box's own disables plugin before the run first enables it, which Hermes's
    `plugins enable` would undo (measured on 0.23.3): each such file and key, or ok. A `plugins.enabled` list
    without it is no switch-off: it is Hermes's opt-in list, which every plugin joins as it is enabled."""
    held = []
    for file, doc in box_layers()[0]:
        disabled = (doc.get("plugins") or {}).get("disabled") if isinstance(doc.get("plugins"), dict) else None
        if isinstance(disabled, list) and plugin in disabled:
            held.append("%s: its plugins.disabled names %s" % (file, plugin))
    return "ok" if not held else "refused " + " and ".join(held)


# WHAT CHECKOUT HANDING ALONE READS OF HERMES'S OWN CODE, each by module, name (dotted for a class's method) and the
# parameters it is called with (None: a constant): checked at every launch (gate). A release that moved one starts the
# run without its checkouts and the mid-run hand, said once by name, since a wrong render or verdict would hand a
# model what Hermes itself would not, and nothing but checkout handing needs them. When another plugin rewrites the
# same tool result, Hermes keeps the first answer, so that clone's instructions are not handed during the run; the
# plugin never orders itself ahead of another. A Hermes release bump re-checks this list and CONTRACT_INTERNALS
# against its source (0.23.3's).
CHECKOUT_INTERNALS = (
    ("agent.prompt_builder", "build_context_files_prompt", ("cwd", "skip_soul", "context_length")),
    ("agent.prompt_builder", "discover_context_files", ("cwd_path",)),
    ("agent.skill_utils", "_skills_cfg_get", ("key",)),
    ("agent.skill_utils", "get_disabled_skill_names", ("platform",)),
    ("agent.skill_utils", "_candidate_project_skills_dirs", ("root",)),
    ("agent.skill_utils", "iter_skill_index_files", ("skills_dir", "filename")),  # mirrored, bounded: skill_index
    ("agent.skill_utils", "iter_project_skill_files", ("project_dir",)),  # mirrored in skill_names
    ("agent.skill_utils", "EXCLUDED_SKILL_DIRS", None),
    ("agent.skill_utils", "SKILL_SUPPORT_DIRS", None),
    ("agent.skill_utils", "_PROJECT_QUARANTINE_CACHE", None),
    ("agent.skill_utils", "is_project_root_trusted", ("root",)),
    ("agent.skill_utils", "is_quarantined_project_skill", ("skill_md",)),
    ("agent.skill_utils", "get_skill_search_roots", ("local", "include_project")),
    ("agent.skill_utils", "parse_frontmatter", ("content",)),
    ("agent.skill_utils", "TIER_PROJECT", None),
    ("agent.model_metadata", "get_model_context_length",
     ("model", "base_url", "api_key", "config_context_length", "provider", "custom_providers")),
    ("agent.agent_init", "_scope_context_length_to_default_runtime",
     ("agent", "_agent_cfg", "_model_cfg", "_custom_providers", "_config_context_length", "base_url")),
    ("hermes_cli.config", "get_compatible_custom_providers", ("config",)),
    ("hermes_cli.config", "get_custom_provider_context_length", ("model", "base_url", "custom_providers")),
    ("hermes_cli.runtime_provider", "resolve_runtime_provider",
     ("requested", "explicit_api_key", "explicit_base_url", "target_model")),
    ("gateway.session_local_route", "resolve_launch_route", ("params", "config")),
    ("hermes_cli.gateway_chat", "env_launch_flags", ()),
    ("hermes_cli.plugins", "_resolve_hook_callback_timeout", ()),
    ("tools.budget_config", "budget_for_context_window", ("context_length",)),
    ("agent.subdirectory_hints", "SubdirectoryHintTracker", ("working_dir",)),
    ("agent.subdirectory_hints", "SubdirectoryHintTracker.check_tool_call", ("tool_name", "tool_args")),
    ("agent.runtime_cwd", "scope_terminal_cwd", ()),
)

# WHAT THE RUN'S CONTRACT READS OF HERMES'S OWN CODE, before and apart from checkout handing (the overlay, the guard,
# the route and the store's carry): judged where each is read, as before, never by the gate. The overlay refuses a
# run it cannot install; the guard's own failure is no verdict; a route or renew it cannot read starts a session of
# the run's own.
CONTRACT_INTERNALS = (
    ("hermes_cli.managed_scope", "get_managed_dir"), ("hermes_cli.managed_scope", "load_managed_config"),
    ("hermes_cli.config", "load_config"), ("hermes_cli.config", "read_raw_config"),
    ("hermes_cli.config", "get_config_path"), ("hermes_cli.config", "DEFAULT_CONFIG"),
    ("hermes_cli.runtime_provider", "resolve_runtime_provider"), ("gateway.session_local_route", "resolve_launch_route"),
    ("hermes_cli.env_loader", "load_hermes_dotenv"), ("hermes_cli.gateway_chat", "env_launch_flags"),
    ("hermes_cli.plugins_discovery", "gate_manifest"), ("hermes_cli.skills_config", "get_disabled_skills"),
    ("tools.mcp_tool_config", "_suspicious_mcp_issues"), ("hermes_cli.tools_config", "_get_platform_tools"),
)


def internals():
    """Each CHECKOUT_INTERNALS entry this Hermes lacks, or whose parameters lack one the run passes, a phrase each."""
    import importlib
    import inspect
    moved = []
    for module, name, params in CHECKOUT_INTERNALS:
        try:
            found = importlib.import_module(module)
            for part in name.split("."):
                found = getattr(found, part)
        except (ImportError, AttributeError):
            moved.append("%s.%s is gone" % (module, name))
            continue
        if params is None:
            continue
        try:
            have = inspect.signature(found).parameters
        except (TypeError, ValueError):
            moved.append("%s.%s is no function" % (module, name))
            continue
        lost = [p for p in params if p not in have]
        if lost:
            moved.append("%s.%s takes no %s" % (module, name, ", ".join(lost)))
    return moved


def launch_window(model):
    """The window Hermes's agent resolves for the run's model as a session starts (agent/agent_init.py, its
    context compressor's get_model_context_length): the launch's route (-m's words, then resolve_runtime_provider, as
    route reads it), model.context_length where it describes that runtime, else a custom provider's, then Hermes's
    own resolution, which falls back to its DEFAULT_FALLBACK_CONTEXT where it cannot place the model. Measured on
    0.23.3 offline under the run's own configuration: a model Hermes's table knows (anthropic/claude-sonnet-4.5 on
    openrouter: 200,000) and one it cannot place (256,000, after its failed metadata fetch) each gave the cut a
    session started in the checkout makes (48,000 and 61,440 characters); online it is the session's figure too."""
    from types import SimpleNamespace
    from agent.agent_init import _scope_context_length_to_default_runtime
    from agent.model_metadata import get_model_context_length
    from gateway.session_local_route import resolve_launch_route
    from hermes_cli.config import get_compatible_custom_providers, get_custom_provider_context_length, load_config
    from hermes_cli.runtime_provider import resolve_runtime_provider
    config = load_config()
    section = config.get("model") if isinstance(config.get("model"), dict) else {}
    launch = resolve_launch_route({"model": model}, config) if model else {}
    runtime = resolve_runtime_provider(requested=launch.get("provider") or None,
                                       explicit_api_key=launch.get("api_key"),
                                       explicit_base_url=launch.get("base_url"), target_model=launch.get("model"))
    agent = SimpleNamespace(model=launch.get("model") or configured_model(config.get("model")),
                            provider=runtime.get("provider"), base_url=runtime.get("base_url"))
    custom = get_compatible_custom_providers(config)
    pinned = section.get("context_length")
    try:
        pinned = int(pinned) if pinned is not None else None
    except (TypeError, ValueError):
        pinned = None
    if pinned is not None:
        pinned = _scope_context_length_to_default_runtime(agent, config, section, custom, pinned,
                                                          launch.get("base_url"))
    if pinned is None and custom:
        pinned = get_custom_provider_context_length(model=agent.model, base_url=agent.base_url,
                                                    custom_providers=custom) or None
    return get_model_context_length(agent.model, base_url=agent.base_url or "", api_key=runtime.get("api_key") or "",
                                    config_context_length=pinned, provider=agent.provider or "",
                                    custom_providers=custom)


# What build_context_files_prompt opens its answer with, which the run's sections replace with their own label.
CONTEXT_HEADER = "# Project Context\n\nThe following project context files have been loaded and should be followed:\n\n"


def checkout_gate(roots, window):
    """The note helper's gate for roots (internal/notehelper's checkoutGate): for each, its instructions as a
    session started there loads them, Hermes's own build_context_files_prompt (one kind, the first with a
    non-empty file; every file through the injection scan; Hermes's cut at window tokens, None for Hermes's
    floor), the files of that kind, and the skill directories such a session loads (only for a root
    skills.trusted_project_dirs names, walked within its bounds ([skill_index]), each passing Hermes's quarantine
    scan of its content now and not disabled), what it leaves out a line each; and every name the home's skills
    are found by. The run's plugin imports it too."""
    from agent import prompt_builder as pb, skill_utils as su
    from hermes_cli.gateway_chat import env_launch_flags
    ignore = bool(env_launch_flags().get("ignore_rules"))
    discovery = su._skills_cfg_get("project_discovery") is not False
    disabled = su.get_disabled_skill_names("cli")
    checkouts = {}
    for root in roots:
        path = Path(root)
        entry = {"text": "", "files": [], "skills": [], "said": []}
        found = pb.discover_context_files(path)
        won = next((kind for kind, _label, _path, content in found if content), None)
        if won is not None and ignore:
            entry["said"].append("the instructions of %s are left out: HERMES_IGNORE_RULES has Hermes load no "
                                 "project's" % root)
        elif won is not None:
            text = pb.build_context_files_prompt(cwd=str(path), skip_soul=True, context_length=window)
            entry["text"] = text[len(CONTEXT_HEADER):] if text.startswith(CONTEXT_HEADER) else text
            entry["files"] = [str(p) for kind, _label, p, content in found if kind == won and content]
        dirs, skills = su._candidate_project_skills_dirs(path.resolve()), []
        if not (discovery and su.is_project_root_trusted(path)):  # TRUST FIRST: an untrusted tree is never walked
            if any_skill(dirs):
                entry["said"].append("the skills of %s are left out: Hermes loads a checkout's skills only where "
                                     "its skills.trusted_project_dirs names the checkout's root" % root)
        else:
            try:
                skills = [md for d in dirs for md in skill_index(d)]
            except WalkBound as err:
                entry["said"].append("the skills of %s are left out: %s" % (root, err))
        for md in skills:
            names = {md.parent.name, str(skill_frontmatter(md).get("name", md.parent.name))}
            if quarantined_now(md):
                entry["said"].append("the skill %s is left out: Hermes's scan quarantines it" % md.parent)
            elif names & disabled:
                entry["said"].append("the skill %s is left out: skills.disabled names it" % md.parent)
            else:
                entry["skills"].append(str(md.parent))
        checkouts[root] = entry
    return {"checkouts": checkouts, "taken": skill_names()}


# A CHECKOUT'S SKILL WALKS ARE BOUNDED as the note helper bounds its own (internal/notehelper's maxWalkDirs and
# maxWalkSkills for a walk, maxCopyFiles for a skill's files): Hermes's iter_skill_index_files follows every link
# unguarded, so a clone linking a directory to itself walks without end (measured on 0.23.3: still running after
# 20 s), which in the run's gateway would spin for the rest of the run. Past a bound the skills are left out, said.
WALK_DIRS, WALK_SKILLS, SKILL_FILES = 4096, 1024, 1024


class WalkBound(Exception):
    """A skill walk past its bound, named in a sentence."""


def skill_index(skills_dir):
    """iter_skill_index_files(skills_dir, "SKILL.md") as Hermes 0.23.3 walks it (links followed, EXCLUDED_SKILL_DIRS
    and a skill's SKILL_SUPPORT_DIRS pruned, sorted), each directory once by device and inode, within WALK_DIRS
    directories read and WALK_SKILLS skills; WalkBound past either."""
    from agent import skill_utils as su
    seen, found, stack = set(), [], [str(skills_dir)]
    while stack:
        top = stack.pop()
        try:
            st = os.stat(top)
            if (st.st_dev, st.st_ino) in seen:
                continue
            seen.add((st.st_dev, st.st_ino))
            entries = list(os.scandir(top))
        except OSError:
            continue
        if len(seen) > WALK_DIRS:
            raise WalkBound("%s holds more than %d directories to walk" % (skills_dir, WALK_DIRS))
        subdirs, files = [], set()
        for e in entries:
            try:
                is_dir = e.is_dir()  # a link to a directory counts, as os.walk's followlinks walk counts it
            except OSError:
                is_dir = False
            if is_dir:
                subdirs.append(e.name)
            else:
                files.add(e.name)
        has_skill = "SKILL.md" in files
        stack += [os.path.join(top, d) for d in subdirs
                  if d not in su.EXCLUDED_SKILL_DIRS and not (has_skill and d in su.SKILL_SUPPORT_DIRS)]
        if has_skill:
            found.append(os.path.join(top, "SKILL.md"))
            if len(found) > WALK_SKILLS:
                raise WalkBound("%s holds more than %d skills" % (skills_dir, WALK_SKILLS))
    return [Path(p) for p in sorted(found)]


def any_skill(dirs):
    """Whether a SKILL.md lies below dirs, for an untrusted root's line: a walk that follows no link, within
    WALK_DIRS directories, past which it answers yes."""
    read = 0
    for d in dirs:
        for _top, _subdirs, files in os.walk(d):
            read += 1
            if "SKILL.md" in files or read >= WALK_DIRS:
                return True
    return False


def quarantined_now(md):
    """Hermes's quarantine verdict on md's skill by its current content: is_quarantined_project_skill, its
    in-process verdict for the skill's directory dropped first, since Hermes keeps that verdict for the process's
    life (measured on 0.23.3) and the plugin renders in the run's gateway, while the scan it runs is keyed by the
    digest of the whole directory, so a re-render judges each skill by what it holds then."""
    from agent import skill_utils as su
    try:
        key = str(md.parent.resolve())
    except OSError:
        key = str(md.parent)
    su._PROJECT_QUARANTINE_CACHE.pop(key, None)
    return su.is_quarantined_project_skill(md)


def checkout_stamp(root):
    """What a render of root reads, as Hermes finds it, or None where it cannot be said within the walks' bounds:
    the root's trust; every context file Hermes's discovery finds there, a [path, mtime_ns, size] each; and, for a
    trusted root, every file below each skill directory, as Hermes's scan reads them, within SKILL_FILES each. The
    run's plugin renders a checkout again where a kept render's stamp no longer matches, or is None."""
    from agent import prompt_builder as pb, skill_utils as su
    path = Path(root)
    trusted = su._skills_cfg_get("project_discovery") is not False and su.is_project_root_trusted(path)
    files = [str(p) for _kind, _label, p, _content in pb.discover_context_files(path)]
    try:
        for md in [md for d in su._candidate_project_skills_dirs(path.resolve()) for md in skill_index(d)] \
                if trusted else []:
            files += skill_files(md.parent)
    except WalkBound:
        return None
    stamp = [trusted]
    for p in files:
        try:
            st = os.stat(p)
            stamp.append([p, st.st_mtime_ns, st.st_size])
        except OSError:
            stamp.append([p, None, None])
    return stamp


def skill_files(skill_dir):
    """Every file below skill_dir, links followed, each directory once, within SKILL_FILES entries; WalkBound
    past it."""
    seen, out = set(), []
    for top, subdirs, files in os.walk(str(skill_dir), followlinks=True):
        try:
            st = os.stat(top)
        except OSError:
            subdirs[:] = []
            continue
        if (st.st_dev, st.st_ino) in seen:
            subdirs[:] = []
            continue
        seen.add((st.st_dev, st.st_ino))
        out += [os.path.join(top, f) for f in files]
        if len(out) + len(seen) > SKILL_FILES:
            raise WalkBound("%s holds more than %d entries" % (skill_dir, SKILL_FILES))
    return sorted(out)


def skill_frontmatter(md):
    """md's frontmatter as Hermes parses it, {} where it cannot be read."""
    from agent.skill_utils import parse_frontmatter
    try:
        front, _body = parse_frontmatter(md.read_text(encoding="utf-8"))
    except Exception:  # an unreadable skill is no name, as Hermes's own catalog skips it
        return {}
    return front if isinstance(front, dict) else {}


def skill_names():
    """Every name a skill the box's Hermes finds is found by (its declared name, its directory's, its path
    under its root: resolve_skill_catalog's aliases), each with that skill's directory, over every root it
    searches, a trusted project's quarantined skills left out as Hermes leaves them; each root walked within its
    bounds ([skill_index]), WalkBound past them."""
    from agent import skill_utils as su
    taken = {}
    for tier, root in su.get_skill_search_roots():
        files = skill_index(root)
        if tier == su.TIER_PROJECT:  # iter_project_skill_files' filter
            files = [md for md in files if not su.is_quarantined_project_skill(md)]
        for md in files:
            aliases = {md.parent.name, str(skill_frontmatter(md).get("name", md.parent.name)),
                       md.parent.relative_to(root).as_posix()}
            for alias in aliases:
                taken.setdefault(alias, str(md.parent))
    return taken


def gate(peek_path, out_path, model, bound):
    """checkout_gate for the roots the note helper's hermes-peek wrote at peek_path, at the window Hermes resolves
    for model (launch_window; asked only where there are roots), written to out_path, under the box's .env as
    Hermes reads it: moved where a CHECKOUT_INTERNALS entry moved (the run starts without its checkouts, said),
    off (the run goes on without its nested hand, said) where plugins.hook_callback_timeout leaves the hand less
    than bound seconds."""
    from hermes_cli.env_loader import load_hermes_dotenv
    load_hermes_dotenv(hermes_home=os.environ.get("HERMES_HOME", ""), load_external_secrets=False)
    moved = internals()
    if moved:
        return "moved " + "; ".join(moved)
    with open(peek_path, encoding="utf-8") as f:
        roots = json.load(f).get("roots") or []
    answer = checkout_gate(roots, launch_window(model)) if roots else {}
    # WRITTEN WHOLE OR NOT AT ALL: a gate killed past its wait leaves no half of one for checkout-launch.
    with open(out_path + ".tmp", "w", encoding="utf-8") as f:
        json.dump(answer, f)
    os.replace(out_path + ".tmp", out_path)
    from hermes_cli.plugins import _resolve_hook_callback_timeout
    hook = _resolve_hook_callback_timeout()
    if 0 < hook < float(bound):
        return ("off plugins.hook_callback_timeout is %g, under the %s seconds the hand may take, so Hermes would "
                "abandon it with the repository claimed (raise it to %s or remove it)" % (hook, bound, bound))
    return "ok"


def guard(plugin, servers, skills):
    """Why the box's Hermes would start without the run's plugin or one of servers' tools, by what Hermes itself
    reads (load_config() under the run's managed scope, its own gates), each reason naming the file, key and
    entry: a sticky profile (main._apply_profile_override's read), a launch switch in an environment or .env, a
    disabled toolset naming the server, a cli toolset list leaving it out, a header beside the server's own, an
    entry Hermes skips (suspicious or a malformed cost table: leaves the run's overlay cannot clear), or the plugin
    gated off, or one of skills (the run's seat skills' names) disabled for the cli, by Hermes's own
    get_disabled_skills, which Hermes leaves unlisted (read in its code). A box file's own switch of one of
    servers (enabled false, disabled true, a tools list leaving out or hiding its tools, servers giving
    each one's tools) refuses it too, though the run's overlay would state it back (measured on 0.23.3)."""
    names = json.loads(servers)
    os.environ.pop("CREWLET_HERMES_PLUGIN", None)  # a plugin Hermes loads while answering registers nothing
    home_dir = os.environ.get("HERMES_HOME", "")
    from hermes_cli.config import get_config_path, load_config
    found = [why for why in (unreadable(str(get_config_path())),) if why]
    if found:
        return "refused " + " | ".join(found)
    config = load_config()
    layers, box_dir = box_layers()

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
    for name, own_entry in names.items():
        sent, tools = own_entry.get("headers") or [], own_entry.get("tools") or []
        for path, holds, what in (
                (("mcp_servers", name, "enabled"), lambda v: v is False, "switches it off"),
                (("mcp_servers", name, "disabled"), lambda v: v is True, "switches it off"),
                (("mcp_servers", name, "tools", "include"), lambda v, t=tools: isinstance(v, list) and (
                    not v or not t or any(not any(fnmatch.fnmatch(n, str(p)) for p in v) for n in t)),
                 "leaves its tools out"),
                (("mcp_servers", name, "tools", "exclude"), lambda v, t=tools: isinstance(v, list) and bool(v) and (
                    not t or any(fnmatch.fnmatch(n, str(p)) for p in v for n in t)), "hides its tools")):
            said = where(path, holds)
            if not said.startswith("Hermes's own"):
                found.append("%s, which %s: the server %s the run's tools go through (remove it)" % (said, what, name))
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


COMMANDS = {"dump": (dump, 4), "trim": (trim, 2), "renew": (renew, 3), "restore": (restore, 4), "overlay": (overlay, 3),
            "top": (top, 1), "requests": (requests, 3), "route": (route, 3), "servers": (servers, 2), "guard": (guard, 3),
            "plugoff": (plugoff, 1), "gate": (gate, 4)}

if __name__ == "__main__":
    command = COMMANDS.get(sys.argv[1] if len(sys.argv) > 1 else "")
    if command is None or len(sys.argv) != command[1] + 2:
        sys.stderr.write("usage: hermes_carry.py dump DB TITLE POLICY OUT | trim SRC DST | renew DB TITLE SYSTEM | "
                         "restore MEMBER DB POLICY TITLE | overlay STAGED MANAGED HOSTS | top DB | requests DB TITLE AFTER | "
                         "route DB TITLE MODEL | "
                         "servers STAGED GUARDED | guard PLUGIN SERVERS SKILLS | plugoff PLUGIN | "
                         "gate PEEK OUT MODEL BOUND\n")
        sys.exit(2)
    sys.stdout.write("crewlet-carry %s\n" % command[0](*sys.argv[2:]))
