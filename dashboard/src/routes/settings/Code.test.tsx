/**
 * Settings › Code reads three answers the engine already serves and draws the
 * code host's part of each. The invariants worth breaking a build over: the
 * alarms it lists are the code host's kinds and no other, the longest-standing
 * first, and a node that evaluated no table is named apart rather than read
 * as quiet; and an unset policy field is drawn as the default it reads as,
 * said to be the default, never as blank.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { CodeRepoSummary, CompanyDocument, FleetAnswer, FleetNode } from "~/protocol/index.ts";
import { RepoProtection, codeAlarmsOf, codePolicyOf, protectionLines } from "./Code.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const node = (id: string, alarms?: FleetNode["alarms"]): FleetNode =>
  ({ id, roles: ["data", "git"], broker: "embedded", ...(alarms ? { alarms } : {}) }) as FleetNode;

const fleet = (nodes: FleetNode[]): FleetAnswer =>
  ({
    nodes,
    seats: [],
    duties: [],
    unplaceable: [],
    unmanned_roles: [],
    this_node: "a",
    target_epoch: 1,
  }) as FleetAnswer;

// THE CODE HOST'S ALARMS AND NO OTHER, the longest-standing first, and a node
// that evaluated no table is listed apart — never as a node with nothing
// firing. Mutation: keep a kind the code host does not raise, drop the sort,
// or count a node with no `alarms` as quiet, and this fails.
test("the alarms are the code host's kinds, oldest first, and a node that said nothing is named apart", () => {
  const got = codeAlarmsOf(
    fleet([
      node("git-1", [
        { kind: "apply_lag", since: "2026-10-30T08:00:00Z" },
        { kind: "code_commit_busy", since: "2026-10-31T09:00:00Z" },
      ]),
      node("git-2", [{ kind: "code_compaction_behind", since: "2026-10-31T07:00:00Z" }]),
      node("booting"),
      node("quiet", []),
    ]),
  );
  expect(got.firing).toEqual([
    { node: "git-2", kind: "code_compaction_behind", since: "2026-10-31T07:00:00Z" },
    { node: "git-1", kind: "code_commit_busy", since: "2026-10-31T09:00:00Z" },
  ]);
  expect(got.silent).toEqual(["booting"]);
});

// AN UNSET FIELD READS AS THE ENGINE'S DEFAULT, and says it is one; a set one
// is the document's value. Mutation: draw an unset field as zero or empty, or
// mark a set one as the default, and this fails.
test("an unset scratch policy and identity domain read as the engine's defaults, marked as defaults", () => {
  const unset = codePolicyOf({ code: { native: true } } as unknown as CompanyDocument);
  expect(unset.idleDays).toEqual({ value: 7, set: false });
  expect(unset.maxPerSeat).toEqual({ value: 50, set: false });
  expect(unset.identityDomain).toEqual({ value: "seats.invalid", set: false });

  const set = codePolicyOf({
    code: {
      native: true,
      scratch: { idle_days: 3, max_per_seat: 0 },
      identity_domain: "acme.example",
    },
  } as unknown as CompanyDocument);
  expect(set.idleDays).toEqual({ value: 3, set: true });
  expect(set.maxPerSeat).toEqual({ value: 0, set: true });
  expect(set.identityDomain).toEqual({ value: "acme.example", set: true });

  expect(codePolicyOf(null).idleDays).toEqual({ value: 7, set: false });
});

// THE GATE PER PATTERN: each rule says what a merge into it must meet, read
// from the repository's detail by its own pattern; a rule the detail gives
// no gate SAYS it has none; and before the detail answers nothing is claimed
// either way. Mutation: match a gate to another rule's pattern, drop the "no
// merge gate" line, or claim none before the detail answered, and this fails.
test("each protected rule says its own gate, or that it has none once the detail answered", () => {
  const rules = [
    { pattern: "refs/heads/main", direct_push: "none" },
    { pattern: "refs/heads/release/*", direct_push: "persons", allow_force: true },
  ];
  const gate = {
    merge_by: "maintainers",
    approvals: "one",
    checks: ["unit"],
    landing: { depth: 4, batch: 4 },
    delete_source: "seats",
  };
  expect(protectionLines(rules, undefined)).toEqual([
    { pattern: "refs/heads/main", says: "nobody pushes to it directly" },
    {
      pattern: "refs/heads/release/*",
      says: "only a person pushes to it directly, forced pushes allowed",
    },
  ]);
  const read = protectionLines(rules, [{ pattern: "refs/heads/main", gate }]);
  expect(read[0]?.says).toBe(
    "nobody pushes to it directly; a merge into it is merged by its maintainers, one approval, 1 check pass, landed by its queue, each change tested merged on the branch before it lands",
  );
  expect(read[1]?.says).toBe(
    "only a person pushes to it directly, forced pushes allowed; no merge gate",
  );
});

/** A declared repository whose main branch takes no direct push. */
const REPO = {
  id: "id-web",
  name: "web",
  status: "active",
  managed_by: "config",
  kind: "project",
  owner: { kind: "unit", ref: "engineering" },
  visibility: "company",
  search: "indexed",
  default_branch: "main",
  protections: [{ pattern: "refs/heads/main", direct_push: "none" }],
} as unknown as CodeRepoSummary;

// A REPOSITORY'S FOLD ASKS ITS DETAIL FOR THE GATES BY NAME: the engine
// answers a repository's question with its row alone unless asked for more,
// and a detail read without them would say "no merge gate" of every rule.
// Mutation: drop `gates` from the question, and this fails.
test("a repository's fold asks for its rules' gates and draws each rule's", async () => {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname !== "/query/code_repo") return new Promise<Response>(() => {});
      const gated = url.searchParams.get("gates") === "true";
      return new Response(
        JSON.stringify({
          repo: {
            ...REPO,
            ...(gated
              ? { gates: [{ pattern: "refs/heads/main", gate: { approvals: "one" } }] }
              : {}),
          },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
  render(<RepoProtection repo={REPO} />);

  expect(await screen.findByText(/a merge into it is .*one approval/)).toBeTruthy();
  const read = asked.find((u) => u.pathname === "/query/code_repo");
  expect(read?.searchParams.get("gates")).toBe("true");
  expect(read?.searchParams.has("path_owners")).toBe(false);
});
