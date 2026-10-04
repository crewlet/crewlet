// @vitest-environment node
/**
 * A new node's identity: what is offered from a name, and what may be recorded.
 *
 * What these protect: an offered handle or key is always one the engine's
 * grammar accepts and no node of the company holds, however odd the name and
 * however long; and an identity that breaks the grammar, or that any seat or
 * unit already answers to, is refused with a sentence saying why.
 */

import { describe, expect, test } from "vitest";
import {
  HANDLE_PATTERN,
  MAX_IDENTITY,
  UNIT_KEY_PATTERN,
  handleProblem,
  mintHandle,
  mintUnitKey,
  slugify,
} from "./identity.ts";

const none = new Set<string>();

describe("offering an identity from a name", () => {
  test("a handle is the name's slug, numbered past every one already held", () => {
    expect(slugify("  QA/Test Lead ")).toBe("qa-test-lead");
    expect(mintHandle("Sarah Chen", none)).toBe("sarah-chen");
    expect(mintHandle("Sarah Chen", new Set(["sarah-chen"]))).toBe("sarah-chen-2");
    expect(mintHandle("Sarah Chen", new Set(["sarah-chen", "sarah-chen-2"]))).toBe("sarah-chen-3");
    // A name with nothing to slug still gets a handle.
    expect(mintHandle("!!!", none)).toBe("seat");
    // A handle may start with a digit; a unit key may not.
    expect(mintHandle("3D Artist", none)).toBe("3d-artist");
  });

  test("a unit key is the name's slug, starting with a letter, numbered past every one held", () => {
    expect(mintUnitKey("Go to Market", none)).toBe("go-to-market");
    expect(mintUnitKey("3D", none)).toBe("unit-3d");
    expect(mintUnitKey("", none)).toBe("unit");
    expect(mintUnitKey("Platform", new Set(["platform"]))).toBe("platform-2");
  });

  test("a long name is cut to the grammar's width, numbered inside it, and never ends in a dash", () => {
    const long = "A".repeat(40) + " " + "B".repeat(40);
    const handle = mintHandle(long, none);
    expect(handle.length).toBeLessThanOrEqual(MAX_IDENTITY);
    expect(handle).toMatch(HANDLE_PATTERN);
    const next = mintHandle(long, new Set([handle]));
    expect(next.length).toBeLessThanOrEqual(MAX_IDENTITY);
    expect(next.endsWith("-2")).toBe(true);
    expect(next).toMatch(HANDLE_PATTERN);
    const key = mintUnitKey("x".repeat(63) + "-y", none);
    expect(key).toMatch(UNIT_KEY_PATTERN);
    expect(key.endsWith("-")).toBe(false);
  });

  test("every offered identity passes the check it will be recorded under", () => {
    const taken = new Set(["ceo", "engineering"]);
    for (const name of ["CEO", "Engineering", "  ", "Ünïcödé Name", "x".repeat(200), "--a--"]) {
      expect(handleProblem(mintHandle(name, taken), taken), name).toBeNull();
    }
  });
});

describe("checking an identity", () => {
  test("a handle breaking the grammar, or held by any seat or unit, is refused naming why", () => {
    const taken = new Set(["dev", "sales"]);
    expect(handleProblem("", taken)).toBe("A seat needs a handle.");
    expect(handleProblem("Dev", taken)).toMatch(/lowercase letters, digits and hyphens/);
    expect(handleProblem("-dev", taken)).toMatch(/lowercase letters, digits and hyphens/);
    expect(handleProblem("a".repeat(MAX_IDENTITY + 1), taken)).toMatch(/at most 64/);
    expect(handleProblem("dev", taken)).toBe("dev already names a seat or a unit.");
    expect(handleProblem("sales", taken)).toBe("sales already names a seat or a unit.");
    expect(handleProblem("dev-2", taken)).toBeNull();
  });
});
