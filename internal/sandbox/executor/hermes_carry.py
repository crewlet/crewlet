# A Hermes Agent 0.23.4 session as text, and back, and the run's managed scope (internal/sandbox/executor's
# hermescarry.go).
#
#   hermes_carry.py dump DB TITLE POLICY OUT        the session titled TITLE, with its compaction lineage,
#                                                   written to OUT
#   hermes_carry.py renew DB TITLE SYSTEM           SYSTEM's text as the standing instructions of the session
#                                                   titled TITLE, in place of the ones Hermes froze with it
#   hermes_carry.py restore MEMBER DB POLICY TITLE  a new session store at DB from MEMBER, the launch kept at
#                                                   POLICY
#   hermes_carry.py overlay STAGED MANAGED          the run's managed scope at MANAGED: the box's own with
#                                                   STAGED's keys over it
#   hermes_carry.py top DB                          the store's newest message id (0 for none), where a
#                                                   launch's own rows begin
#   hermes_carry.py route DB TITLE MODEL            whether the box's model route (provider, base URL, model)
#                                                   is the one Hermes froze with the session titled TITLE
#
# Run under Hermes's own interpreter through its launcher (`hermes --run-module cProfile -o /dev/null`),
# which keeps no exit status, so every command ends on one line of its own on stdout: `crewlet-carry ok`
# (with the restored session's stored id after a restore, the id after top), or `crewlet-carry none` where
# the store holds no session of that title (or, for renew, no launch Hermes froze with it; for route, no
# route), and route answers `crewlet-carry moved` for another route. Anything else is a failure, its traceback
# on stderr.
#
# A dump is JSON lines: the store's schema ({"ddl", "kind"}, never the full-text index's own tables, which
# its triggers fill again), the store's own bookkeeping rows whole, then the session's rows ({"t", "r"}),
# and the launch Hermes froze with it ({"crewlet_policy"}). Text, so the engine's scrub can rewrite it.
# Standard library only, but for overlay and route, which read the box's configuration through Hermes's own
# modules.
import base64
import copy
import json
import os
import sqlite3
import sys

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
    """Whether a child session is its parent's compaction continuation, as 0.23.4's
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
    whose compression.in_place is false has Hermes rotate a session on compaction (measured on 0.23.4): the
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
        # Written back as Hermes writes both (measured on 0.23.4): the environment spaced with non-ASCII
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
    (measured on 0.23.4: a resume under another provider and base URL still calls the frozen one), so a
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


def overlay(staged_path, managed_dir):
    """The run's HERMES_MANAGED_DIR hides the box's own managed scope (Hermes's default, /etc/hermes: an
    administrator's config.yaml and .env), so MANAGED holds that scope, read through Hermes's loader as
    Hermes reads it, with STAGED's keys over its config.yaml. A list is one leaf, so a run that disables
    toolsets disables the box's effective list (its config.yaml under that scope) and its own."""
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
    os.makedirs(managed_dir, exist_ok=True)
    put(os.path.join(managed_dir, ".env"), env.read_bytes() if env is not None and env.is_file() else None)
    put(os.path.join(managed_dir, "config.yaml"), json.dumps(config).encode())
    return "ok"


COMMANDS = {"dump": (dump, 4), "renew": (renew, 3), "restore": (restore, 4), "overlay": (overlay, 2), "top": (top, 1),
            "route": (route, 3)}

if __name__ == "__main__":
    command = COMMANDS.get(sys.argv[1] if len(sys.argv) > 1 else "")
    if command is None or len(sys.argv) != command[1] + 2:
        sys.stderr.write("usage: hermes_carry.py dump DB TITLE POLICY OUT | renew DB TITLE SYSTEM | "
                         "restore MEMBER DB POLICY TITLE | overlay STAGED MANAGED | top DB | route DB TITLE MODEL\n")
        sys.exit(2)
    sys.stdout.write("crewlet-carry %s\n" % command[0](*sys.argv[2:]))
