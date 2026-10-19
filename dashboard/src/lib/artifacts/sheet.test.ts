import { describe, expect, test } from "vitest";
import { csvFileName } from "./sheet.ts";

describe("a collection's CSV", () => {
  test("is named by the artifact's and the collection's titles, never the key", () => {
    expect(csvFileName("Q4 launch review", "Launch gates")).toBe(
      "Q4 launch review - Launch gates.csv",
    );
  });

  test("drops what a file system refuses and says one title once", () => {
    expect(csvFileName('Plan: "A/B"\ttest', 'Plan: "A/B" test')).toBe("Plan A B test.csv");
    expect(csvFileName("Budget", "")).toBe("Budget.csv");
    expect(csvFileName("", "")).toBe("Export.csv");
  });

  test("is cut to a length every system keeps", () => {
    expect(csvFileName("x".repeat(300), "y").length).toBeLessThanOrEqual(124);
  });
});
