/**
 * `next` is an address on THIS page, or it is nothing.
 *
 * It is the one parameter of a sign-in an outsider can choose for somebody
 * else — a `#/login?next=…` link in a message — so a sign-in that forwarded
 * wherever it said would be somebody else's redirect with this product's
 * trust behind it.
 */

// @vitest-environment node

import { describe, expect, test } from "vitest";
import { LANDING, safeNext, signInHash } from "./session.ts";

describe("where a sign-in may send the reader", () => {
  // THE CONTROL: the addresses this product writes itself survive whole,
  // path and query, or the guard would send every reader to the Inbox and
  // look like it worked.
  test("a route of this dashboard is followed, path and query", () => {
    expect(safeNext("#/work/ENG-42")).toBe("#/work/ENG-42");
    expect(safeNext("#/work?view=board&status=open")).toBe("#/work?view=board&status=open");
    expect(safeNext("#/")).toBe("#/");
    expect(safeNext("#/knowledge/ENG/Deploy%20runbook")).toBe("#/knowledge/ENG/Deploy%20runbook");
  });

  test.each([
    ["an absolute address", "https://evil.example/phish"],
    ["a protocol-relative address", "//evil.example"],
    ["a path on this host", "/dashboard"],
    ["a scheme", "javascript:alert(1)"],
    ["a data URL", "data:text/html,<script>alert(1)</script>"],
    ["a backslash a browser reads as a slash", "#/\\evil.example"],
    ["a protocol-relative hash", "#//evil.example"],
    ["a tab a browser strips before it parses", "#/\tevil"],
    ["a newline", "#/work\n//evil.example"],
    ["a bare word", "evil"],
    ["nothing", ""],
    ["a value far past any address", `#/${"a".repeat(5000)}`],
  ])("%s lands on the Inbox instead", (_what, raw) => {
    expect(safeNext(raw)).toBe(LANDING);
  });

  // A SIGN-IN THAT ANSWERS WITH A SIGN-IN is a loop the reader cannot leave.
  test("a sign-in screen is never where a sign-in ends", () => {
    expect(safeNext("#/login?next=%23%2Fwork")).toBe(LANDING);
    expect(safeNext("#/invite/abc.def")).toBe(LANDING);
    expect(safeNext("#/enrol")).toBe(LANDING);
    expect(safeNext(null)).toBe(LANDING);
  });

  test("what is followed is the router's own spelling of it", () => {
    // A segment is decoded and re-encoded, so a percent sequence the reader
    // was never shown cannot survive as something else.
    expect(safeNext("#/work/a%2Fb")).toBe("#/work/a%2Fb");
  });
});

describe("the address of the sign-in screen", () => {
  test("it carries where the reader was", () => {
    expect(signInHash("#/work/ENG-42?thread=history")).toBe(
      `#/login?next=${encodeURIComponent("#/work/ENG-42?thread=history")}`,
    );
  });

  // AN ENROLMENT INTERRUPTED BY A LOST SESSION still ends where it was
  // going, rather than at a sign-in that sends the reader back to itself.
  test("from a sign-in screen it carries that screen's own destination", () => {
    const from = `#/enrol?next=${encodeURIComponent("#/admin/config")}`;
    expect(signInHash(from)).toBe(`#/login?next=${encodeURIComponent("#/admin/config")}`);
  });

  test("and never an address the sign-in would refuse to follow", () => {
    expect(signInHash("#/login?next=https%3A%2F%2Fevil.example")).toBe(
      `#/login?next=${encodeURIComponent("#/")}`,
    );
  });
});
