// THE one definition of where the built dashboard is.
//
// CREWLET_DASHBOARD_ROOT names the tree to test. Unset, it is
// `static/dashboard` at the repository root: the tree the binary embeds, which
// is the one that matters — this replay is the compatibility reference for the
// client's half of the wire protocol, so it has to run against the bytes a
// browser is actually served.
//
// The Go runner sets the variable explicitly rather than relying on that
// default, so the gate never depends on a relative path staying true.
//
// Resolution is CHECKED rather than assumed. A wrong root would otherwise
// surface as ERR_MODULE_NOT_FOUND on the first import, which reads as a missing
// dashboard file rather than as a missing dashboard.

import { existsSync } from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

/** The tree this repository ships, relative to this file. */
const IN_REPO = join(HERE, "../../../static/dashboard");

const named = process.env.CREWLET_DASHBOARD_ROOT;

/**
 * The dashboard tree under test, as a filesystem path.
 *
 * Resolved against the REPOSITORY rather than the process's working directory
 * when it is relative: `node tests/dashboard/js/replay.mjs` and
 * `cd tests/dashboard/js && node replay.mjs` must name the same tree, and cwd
 * does not survive that.
 */
export const DASHBOARD_DIR = named
  ? isAbsolute(named)
    ? resolve(named)
    : resolve(HERE, named)
  : resolve(IN_REPO);

if (!existsSync(join(DASHBOARD_DIR, "index.html"))) {
  throw new Error(
    `no dashboard at ${DASHBOARD_DIR} (no index.html there).\n` +
      `Set CREWLET_DASHBOARD_ROOT to the tree to test, or leave it unset to ` +
      `use the one in this checkout. If it is missing entirely, the built ` +
      `dashboard has not been committed — run \`make dashboard\`.`,
  );
}

/**
 * The wire protocol, as plain ESM.
 *
 * A SEPARATE build target from the application bundle (dashboard/
 * vite.protocol.config.ts): the app bundle is minified, code-split and boots
 * React against a DOM, none of which a Node process has. This one is the same
 * source, emitted once more unminified, so the replay runs the client's REAL
 * dispatch table rather than a re-implementation of it that could agree with a
 * server the dashboard does not.
 */
export const PROTOCOL_URL = pathToFileURL(join(DASHBOARD_DIR, "protocol.js"));

if (!existsSync(join(DASHBOARD_DIR, "protocol.js"))) {
  throw new Error(
    `no protocol.js in ${DASHBOARD_DIR}. It is a second Vite build target and ` +
      `easy to lose: \`npm run build\` in dashboard/ emits both.`,
  );
}
