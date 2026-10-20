import { expect, test } from "vitest";
import type { WorkProjectRow } from "~/protocol/index.ts";
import { projectOptions } from "./startingProject.ts";

const row = (key: string, name: string) => ({ key, name }) as WorkProjectRow;

// A NAME THAT IS ITS KEY is said once: the Project field read "ENG · ENG".
test("a project is its key and its name, and a name that is its key is said once", () => {
  expect(
    projectOptions([row("ENG", "Core platform"), row("OPS", "OPS"), row("DES", "")], "").map(
      (o) => o.label,
    ),
  ).toEqual(["ENG · Core platform", "OPS", "DES"]);
});

test("a project the door named before the list answered is offered as itself", () => {
  expect(projectOptions([row("ENG", "Core platform")], "GONE")[0]).toMatchObject({
    value: "GONE",
    label: "GONE",
  });
});
