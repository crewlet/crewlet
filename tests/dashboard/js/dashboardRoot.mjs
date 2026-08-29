// THE one definition of where the dashboard tree is.
//
// Every suite in this directory reads the shipped dashboard off disk — its
// modules by dynamic import, its stylesheets and its shell by readFileSync —
// and each one used to spell that location itself as a relative literal.
// Thirty-five copies of one fact, which is the shape of thing this project
// collapses everywhere else it appears (the topic grammar, the ${VAR}
// grammar, the .env assignment grammar), and for the same reason: a location
// that is written down in many places is a location that can only ever be
// moved in most of them.
//
// It has since been moved twice — which is the argument, made twice.
//
// CREWLET_DASHBOARD_ROOT names the tree to test. Unset, it is `static/dashboard`
// at the repository root: the tree the binary embeds, which is the one that
// matters, since these suites are the compatibility reference for the client's
// half of the wire protocol. The Go runner sets the variable explicitly rather
// than relying on that default, so the gate never depends on a relative path
// staying true.
//
// Resolution is CHECKED rather than assumed. A wrong root would otherwise
// surface as `ERR_MODULE_NOT_FOUND` on whichever module a suite imported
// first, which reads as a missing dashboard file rather than as a missing
// dashboard.

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
 * Resolved against the *repository* rather than the process's working
 * directory when it is relative: `node tests/test_dashboard/js/store.test.mjs`
 * and `cd tests/test_dashboard/js && node store.test.mjs` must name the same
 * tree, and cwd does not survive that.
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
      `use the one in this checkout.`,
  );
}

/**
 * The same tree as a URL, with the trailing slash a base needs.
 *
 * `new URL("js/store.js", DASHBOARD_URL)` resolves; without the slash the last
 * segment is a filename and gets replaced, which silently yields a sibling of
 * the dashboard rather than a file inside it.
 */
export const DASHBOARD_URL = pathToFileURL(DASHBOARD_DIR + "/");

/** The module directory, the base most suites actually want. */
export const JS_URL = new URL("js/", DASHBOARD_URL);

/** The stylesheet directory, for the suites that fetch a sheet by URL. */
export const STYLES_URL = new URL("styles/", DASHBOARD_URL);

/** The module directory as a filesystem path, for the suites that read source. */
export const JS_DIR = join(DASHBOARD_DIR, "js");

/** The stylesheet directory as a filesystem path. */
export const STYLES_DIR = join(DASHBOARD_DIR, "styles");
