// The check a Copilot run's script makes, with the box's node, that a launch
// attempt Copilot refused left nothing in its session store before the run is
// started again (copilotrepo.go, copilotAttempts): `node <this> <db> <id>`.
//
// Copilot 1.0.114 keeps every session it starts in $COPILOT_HOME's
// session-store.db, a refused one too, which the model's session_store_sql
// tool reads; the run's extension deletes a refused attempt's session
// (copilot_steer.js). This counts the session's id in `sessions.id` and in
// every table keyed by `session_id` (turns, search_index, checkpoints,
// session_files, session_refs, forge_trajectory_events,
// assistant_usage_events, measured), reading the database read-only: a WAL
// reader writes nothing and blocks nothing (measured). No database holds
// nothing. Exit 0 where nothing holds it; otherwise one stream line, a
// `crewlet.refused` naming what was found or why the store could not be read,
// and exit 1.

import { existsSync } from "node:fs"
import { DatabaseSync } from "node:sqlite"

const [db, id] = process.argv.slice(2)

function refuse(message) {
  process.stdout.write(JSON.stringify({ type: "crewlet.refused", data: { reason: "store", session: id, message } }) + "\n")
  process.exit(1)
}

if (typeof db !== "string" || typeof id !== "string" || id === "") refuse("the check was given no store or session")
if (existsSync(db)) {
  const found = []
  try {
    const store = new DatabaseSync(db, { readOnly: true })
    const count = (sql) => store.prepare(sql).get(id).n
    const quote = (name) => '"' + name.replaceAll('"', '""') + '"'
    const sessions = count("select count(*) as n from sessions where id = ?")
    if (sessions > 0) found.push(`sessions ${sessions}`)
    for (const { name } of store.prepare("select name from sqlite_master where type = 'table' order by name").all()) {
      const columns = store.prepare(`pragma table_info(${quote(name)})`).all()
      if (!columns.some((c) => c.name === "session_id")) continue
      const n = count(`select count(*) as n from ${quote(name)} where session_id = ?`)
      if (n > 0) found.push(`${name} ${n}`)
    }
    store.close()
  } catch (error) {
    refuse(`${db} could not be read: ${error?.message ?? error}`)
  }
  if (found.length > 0) refuse(`${db} still holds it: ${found.join(", ")}`)
}
