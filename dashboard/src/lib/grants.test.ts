import { expect, test } from "vitest";
import { isAccessOption, requestOption } from "./grants.ts";

// A REQUEST'S OPTION IS NO CATALOGUE KEY: both carry the engine's reserved
// prefix, and the Inbox draws them two ways, so each is told from the other
// and a request's option reads back as its id and its length. Mutation: test
// the prefix alone and a request's option opens the catalogue's preview.
test("a request's grant option reads as its request, and never as a catalogue grant", () => {
  expect(requestOption("grant-rabcdefghij-t")).toEqual({ id: "abcdefghij", permanent: false });
  expect(requestOption("grant-rabcdefghij-p")).toEqual({ id: "abcdefghij", permanent: true });
  expect(isAccessOption("grant-rabcdefghij-t")).toBe(false);
  expect(isAccessOption("grant-health")).toBe(true);
  expect(requestOption("grant-health")).toBeNull();
  expect(requestOption("grant-rshort-t")).toBeNull();
});
