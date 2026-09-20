// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * A query whose parameter is not chosen yet must not be ASKED.
 *
 * # Why this is a test and not a convention
 *
 * `useQuery(what, params)` sends the frame whether `params` is an object or
 * `undefined` — an absent params object is an EMPTY one on the wire, not a
 * skipped question. So the idiom every screen reaches for,
 *
 *     useQuery("work_project", project ? { key: project } : undefined, …)
 *
 * reads like a guard and is not one. The engine refuses a question missing a
 * parameter it has no default for (`queries: bad parameters: key is
 * required`), and the refusal is invisible where it is made: the screen keeps
 * its last answer and renders an error for a question nobody asked, while the
 * ENGINE logs it once per poll — for the whole time a board sits on its
 * default view. The guard that works is `enabled`, and it is one word away
 * from the guard that does not.
 *
 * Three of the tree's nine conditional call sites had it wrong when this was
 * written — `work_project` on the board (which polls at a minute and whose
 * parameter is empty until somebody picks a project, so it fired for ever),
 * and `work_my_work` among them — while six had it right. That ratio is
 * what makes this a test: the correct and incorrect forms differ by an option
 * nobody misses in review, and nothing else in the build can tell them apart.
 *
 * # What it cannot see
 *
 * A parameter that is required by the engine but always non-empty at the call
 * site needs no guard and gets none — `{ id }` on a screen the router only
 * reaches with an id. This checks the shape that ADMITS it may be empty, which
 * is the shape that is wrong without `enabled`.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));

interface Call {
  what: string;
  where: string;
  guarded: boolean;
}

/**
 * Every `useQuery` call whose params may be `undefined`.
 *
 * The call text is taken by COUNTING BRACKETS from `useQuery(` to its own
 * close, rather than by one regex over the whole call. Two reasons, and the
 * second is the one that matters: prettier breaks exactly these calls over
 * several lines, so a line-at-a-time scan sees none of them — and a regex has
 * to be told how far to look, which means deciding in advance that the options
 * object is there. It is optional on the hook, and a conditional params
 * argument passed with NO options at all is the shape that cannot possibly be
 * guarded. Bounding the match would have skipped exactly that one.
 */
function conditionalCalls(file: string, text: string): Call[] {
  const found: Call[] = [];
  const opens = /useQuery\(\s*"([a-z_0-9]+)"\s*,/g;
  for (const m of text.matchAll(opens)) {
    const from = m.index + m[0].length;
    const body = callBody(text, from);
    // The body stops before the call's own `)`, so a conditional params
    // argument with no options after it ends AT `undefined`.
    if (body === null || !/:\s*undefined\s*(?:,|$)/.test(body.trim())) continue;
    found.push({
      what: m[1] ?? "",
      where: `${file}:${text.slice(0, m.index).split("\n").length}`,
      guarded: /\benabled\s*:/.test(body),
    });
  }
  return found;
}

/** Everything from `from` up to the `)` that closes the call it is inside. */
function callBody(text: string, from: number): string | null {
  let depth = 1;
  for (let i = from; i < text.length; i++) {
    const c = text[i];
    if (c === "(" || c === "{" || c === "[") depth++;
    else if (c === ")" || c === "}" || c === "]") {
      depth--;
      if (depth === 0) return text.slice(from, i);
    }
  }
  return null;
}

function sources(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) sources(p, out);
    else if (/\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry)) out.push(p);
  }
  return out;
}

describe("a query that may have no parameters", () => {
  const all = sources(SRC).flatMap((p) =>
    conditionalCalls(p.slice(SRC.length), readFileSync(p, "utf8")),
  );

  test("passes `enabled` so it is skipped rather than refused", () => {
    const unguarded = all.filter((c) => !c.guarded);
    expect(unguarded.map((c) => `${c.what} — ${c.where}`)).toEqual([]);
  });

  test("is found at all, so the scan cannot pass by reading nothing", () => {
    // Vacuous if the pattern stops matching — which a reformat of these
    // calls, a renamed hook or a moved source root would each do silently.
    expect(all.length).toBeGreaterThanOrEqual(3);
    expect(all.some((c) => c.what === "work_project")).toBe(true);
  });

  test("is caught even when the call passes no options at all", () => {
    // The shape a bounded regex skipped, and the worst one: no options
    // object means there is nowhere for `enabled` to be.
    const bare = conditionalCalls(
      "x.tsx",
      `const a = useQuery("work_project", key ? { key } : undefined);`,
    );
    expect(bare.map((c) => ({ what: c.what, guarded: c.guarded }))).toEqual([
      { what: "work_project", guarded: false },
    ]);
  });

  test("and a guarded call over several lines is read as guarded", () => {
    const wrapped = conditionalCalls(
      "x.tsx",
      `const a = useQuery("work_project", chosen ? { project: chosen } : undefined, {
         enabled: chosen !== "",
         pollMs: 60_000,
       });`,
    );
    expect(wrapped.map((c) => c.guarded)).toEqual([true]);
  });
});
