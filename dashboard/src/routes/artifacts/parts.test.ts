import { expect, test } from "vitest";
import { artifactAddress } from "./parts.tsx";

const ID = "0192f2c4-4444-7000-8000-000000000004";

test("an artifact's address is its page, at the version on show where one is named", () => {
  const page = `${location.origin}${location.pathname}#/artifacts/${ID}`;
  expect(artifactAddress(ID)).toBe(page);
  expect(artifactAddress(ID, 3)).toBe(`${page}?version=3`);
});
