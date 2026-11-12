import { describe, expect, it } from "vitest";

import { andList, orList } from "./words.ts";

describe("andList", () => {
  // THE ENGINE'S RULE BY DEFAULT, so a list the harness says through
  // `internal/words` reads the same on a screen. Mutation: add the serial
  // comma by default and the three-item case fails.
  it("says a list the way the harness does", () => {
    expect(andList([])).toBe("");
    expect(andList(["a"])).toBe("a");
    expect(andList(["a", "b"])).toBe("a and b");
    expect(andList(["a", "b", "c"])).toBe("a, b and c");
  });

  // A CLAUSE CARRYING ITS OWN "and" keeps the comma before the last one, or
  // the last two read as one item.
  it("keeps the serial comma where a clause carries its own and", () => {
    expect(andList(["a", "b"], { serialComma: true })).toBe("a and b");
    expect(andList(["a", "b", "c and d"], { serialComma: true })).toBe("a, b, and c and d");
  });
});

describe("orList", () => {
  // A CHOICE IS SAID BY andList'S RULE, the last item after "or" and no
  // serial comma. Mutation: join it with "and" and every case past one item
  // fails.
  it("says a choice among a list's items", () => {
    expect(orList([])).toBe("");
    expect(orList(["a"])).toBe("a");
    expect(orList(["a", "b"])).toBe("a or b");
    expect(orList(["No access", "Read-only", "Read and write"])).toBe(
      "No access, Read-only or Read and write",
    );
  });
});
