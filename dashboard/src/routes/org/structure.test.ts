import { expect, test } from "vitest";
import { findSeats, movedTo } from "./Structure.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { CHART_ORG } from "~/test/orgchart.ts";

const q = (s: string) => new URLSearchParams(s);

// AN EARLIER ORG PAGE'S ADDRESS OPENS THE TAB THAT HOLDS IT NOW, with what its
// query asked for said in Structure's terms: Edit org naming a node still
// opens that node's editor, as the link meant; its table view is the Table;
// the Roster and Teams are the Table, keeping an open panel, and a team's page
// is the Table with that team's panel open.
//
// Mutation: drop `editor=open` from an Edit org link, keep `view=`, or lose the
// team, and this fails.
test("an earlier org page's address says where its content is now", () => {
  expect(movedTo("edit", undefined, q("seat=pm"))).toEqual({
    path: ["company", "org"],
    query: { seat: "pm", editor: "open" },
  });
  expect(movedTo("edit", undefined, q("view=table&unit=core"))).toEqual({
    path: ["company", "org", "table"],
    query: { unit: "core", editor: "open" },
  });
  expect(movedTo("edit", undefined, q("add=agent&unit=core"))).toEqual({
    path: ["company", "org"],
    query: { add: "agent", unit: "core" },
  });
  expect(movedTo("roster", undefined, q("peek=seat:pm&group=state"))).toEqual({
    path: ["company", "org", "table"],
    query: { peek: "seat:pm" },
  });
  expect(movedTo("teams", "core", q(""))).toEqual({
    path: ["company", "org", "table"],
    query: { peek: "unit:core" },
  });
});

test("the finder ranks a name or handle that starts with the term first", () => {
  const index = indexOrg(CHART_ORG);
  const first = index.seats[0]!;
  const term = first.name.slice(0, 3);
  expect(findSeats(index, term)[0]?.name.toLowerCase().startsWith(term.toLowerCase())).toBe(true);
  expect(findSeats(index, "   ")).toEqual([]);
});
