# A Hermes Agent 0.23.3 session as text, and back (internal/sandbox/executor's hermescarry.go).
#
#   hermes_carry.py dump DB TITLE POLICY OUT        the session titled TITLE, with its compaction lineage,
#                                                   written to OUT
#   hermes_carry.py renew DB TITLE SYSTEM           SYSTEM's text as the standing instructions of the session
#                                                   titled TITLE, in place of the ones Hermes froze with it
#   hermes_carry.py restore MEMBER DB POLICY TITLE  a new session store at DB from MEMBER, the launch kept at
#                                                   POLICY
#
# Run under Hermes's own interpreter through its launcher (`hermes --run-module cProfile -o /dev/null`),
# which keeps no exit status, so every command ends on one line of its own on stdout: `crewlet-carry ok`
# (with the restored session's stored id after a restore), or `crewlet-carry none` where the store holds
# no session of that title. Anything else is a failure, its traceback on stderr.
#
# A dump is JSON lines: the store's schema ({"ddl", "kind"}, never the full-text index's own tables, which
# its triggers fill again), the store's own bookkeeping rows whole, then the session's rows ({"t", "r"}),
# and the launch Hermes froze with it ({"crewlet_policy"}). Text, so the engine's scrub can rewrite it.
# Standard library only.
import base64
import json
import os
import sqlite3
import sys

WHOLE = ("schema_version", "state_meta", "runtime_epoch")
SESSION_ROWS = (("messages", "session_id"), ("session_model_usage", "session_id"),
                ("session_admissions", "target_session_id"))
INDEX_SUFFIXES = ("_config", "_data", "_docsize", "_idx", "_content")


def enc(v):
    return {"$b64": base64.b64encode(v).decode()} if isinstance(v, bytes) else v


def dec(v):
    return base64.b64decode(v["$b64"]) if isinstance(v, dict) and "$b64" in v else v


def dump(db_path, title, policy_path, out_path):
    db = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)
    db.row_factory = sqlite3.Row
    root = db.execute("select id from sessions where title = ? order by started_at limit 1", (title,)).fetchone()
    if root is None:
        return "none"
    ids = [root[0]]
    for sid in ids:
        ids.extend(r[0] for r in db.execute("select id from sessions where parent_session_id = ? order by started_at",
                                            (sid,)) if r[0] not in ids)
    out = open(out_path, "w", encoding="utf-8")
    put = lambda o: out.write(json.dumps(o, ensure_ascii=False) + "\n")
    virtual = {r[0] for r in db.execute("select name from sqlite_master where type = 'table' and sql like 'CREATE VIRTUAL TABLE%'")}
    for kind, name, sql in db.execute("select type, name, sql from sqlite_master where sql is not null order by "
                                      "case type when 'table' then 0 when 'index' then 1 else 2 end, rowid"):
        if name == "sqlite_sequence" or (kind == "table" and any(name == v + s for v in virtual for s in INDEX_SUFFIXES)):
            continue
        put({"ddl": sql, "kind": kind})
    rows = lambda q, a=(): [dict(r) for r in db.execute(q, a)]
    for table in WHOLE:
        for r in rows('select * from "%s"' % table):
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


POLICY_KEY = "gateway.local_policy.v1:%s"
SYSTEM_ENV = "HERMES_EPHEMERAL_SYSTEM_PROMPT"


def renew(db_path, title, system_path):
    """Hermes freezes a session's client environment, the standing instructions among it, in its local
    policy (state_meta) as it creates the session, and a resume keeps it; the run's own are written there
    before the resume, with the gateway stopped."""
    try:
        with open(system_path, encoding="utf-8") as f:
            system = f.read()
    except FileNotFoundError:
        system = ""
    db = sqlite3.connect(db_path)
    root = db.execute("select id from sessions where title = ? order by started_at limit 1", (title,)).fetchone()
    if root is None:
        return "none"
    ids = [root[0]]
    for sid in ids:
        ids.extend(r[0] for r in db.execute("select id from sessions where parent_session_id = ?", (sid,)) if r[0] not in ids)
    for sid in ids:
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
        policy["client_env_json"] = json.dumps(env, sort_keys=True)
        local["policy"] = policy
        db.execute("update state_meta set value = ? where key = ?", (json.dumps(local), POLICY_KEY % sid))
    db.commit()
    return "ok"


COMMANDS = {"dump": (dump, 4), "renew": (renew, 3), "restore": (restore, 4)}

if __name__ == "__main__":
    command = COMMANDS.get(sys.argv[1] if len(sys.argv) > 1 else "")
    if command is None or len(sys.argv) != command[1] + 2:
        sys.stderr.write("usage: hermes_carry.py dump DB TITLE POLICY OUT | renew DB TITLE SYSTEM | "
                         "restore MEMBER DB POLICY TITLE\n")
        sys.exit(2)
    sys.stdout.write("crewlet-carry %s\n" % command[0](*sys.argv[2:]))
