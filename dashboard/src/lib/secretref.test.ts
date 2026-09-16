import { expect, test } from "vitest";

import { complete, rank, referenceAt } from "./secretref.ts";

// WHAT IS UNDER THE CARET, which is not the same as what the box holds.
//
// A field can hold a reference among other text, and the one being completed
// is the one being edited. Matching the whole value instead would offer a
// list while somebody was typing at the far end of a value they had finished.
test("a reference is found from the caret back, and only there", () => {
  // The plain case: typing `$` opens one with nothing typed of it yet.
  expect(referenceAt("$", 1)).toEqual({ start: 0, query: "" });
  expect(referenceAt("${", 2)).toEqual({ start: 0, query: "" });
  expect(referenceAt("${GITHUB", 8)).toEqual({ start: 0, query: "GITHUB" });

  // Among other text, and from where the caret actually is rather than from
  // the end of the string.
  expect(referenceAt("https://${SITE", 14)).toEqual({ start: 8, query: "SITE" });
  expect(referenceAt("${A} and ${B", 12)).toEqual({ start: 9, query: "B" });

  // A CLOSED REFERENCE IS FINISHED. Offering to complete it again would
  // replace a name somebody had already chosen.
  expect(referenceAt("${GITHUB_TOKEN}", 15)).toBeNull();
  // A SPACE ENDS THE SCAN, because no name has one and the `$` before it
  // belongs to a different word.
  expect(referenceAt("$A B", 4)).toBeNull();
  // A DOLLAR IN A VALUE IS NOT A REFERENCE. A password may hold one, and a
  // list opening over it would be offering to replace what was typed.
  expect(referenceAt("pa$$w0rd!", 9)).toBeNull();
  expect(referenceAt("no dollar here", 14)).toBeNull();
});

// CLOSEST FIRST, which is the order somebody means them in.
test("names are ranked by how closely they match", () => {
  const names = ["ATLASSIAN_ORG_ID", "GITHUB_TOKEN", "GH_WEBHOOK", "DATADOG_APP_KEY"];

  // A PREFIX BEATS A SUBSTRING BEATS A SUBSEQUENCE.
  expect(rank(names, "GH")).toEqual(["GH_WEBHOOK", "GITHUB_TOKEN"]);
  // Case is ignored: the names are upper snake and nobody holds shift to
  // filter a list.
  expect(rank(names, "token")).toEqual(["GITHUB_TOKEN"]);
  // A subsequence still matches, last: "dak" is in DATADOG_APP_KEY in order.
  expect(rank(names, "dak")).toEqual(["DATADOG_APP_KEY"]);
  // NOTHING MATCHES NOTHING, rather than falling back to the whole list: an
  // empty list is what closes the popup, and a query with no answer must not
  // leave every name on offer.
  expect(rank(names, "zzz")).toEqual([]);
  // AND `$` ALONE SHOWS EVERYTHING, in the order it arrived, which is the
  // whole point of typing it.
  expect(rank(names, "")).toEqual(names);
});

// A CHOSEN NAME IS WRITTEN WHOLE, braces and all.
//
// The engine resolves a reference only when it is complete; a half-written
// one is a literal that reaches a vendor as the characters somebody typed.
test("choosing a name writes a whole reference and leaves the caret after it", () => {
  const typed = referenceAt("${GITH", 6);
  if (!typed) throw new Error("nothing under the caret");
  expect(complete("${GITH", 6, typed, "GITHUB_TOKEN")).toEqual({
    value: "${GITHUB_TOKEN}",
    caret: 15,
  });

  // WHAT FOLLOWS IS KEPT, and a `}` somebody had already typed is not
  // doubled: `${GITH}` completed would otherwise become `${GITHUB_TOKEN}}`.
  const closed = referenceAt("${GITH}", 6);
  if (!closed) throw new Error("nothing under the caret");
  expect(complete("${GITH}", 6, closed, "GITHUB_TOKEN").value).toBe("${GITHUB_TOKEN}");

  // Among other text, both sides survive.
  const inline = referenceAt("https://$SITE/x", 13);
  if (!inline) throw new Error("nothing under the caret");
  expect(complete("https://$SITE/x", 13, inline, "SITE_URL").value).toBe("https://${SITE_URL}/x");
});
