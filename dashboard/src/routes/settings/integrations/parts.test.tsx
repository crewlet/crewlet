/**
 * A connected integration's Settings tab: what each setting holds, in the face
 * its kind is read in.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import type { SetupRequirement } from "~/protocol/index.ts";
import { SettingsSummary } from "./parts.tsx";
import type { Tool } from "./model.ts";

afterEach(cleanup);

function req(over: Partial<SetupRequirement>): SetupRequirement {
  return {
    field: "f",
    label: "F",
    kind: "text",
    config_path: "integrations.github.f",
    required: false,
    present: true,
    ...over,
  };
}

// A CHOICE IS READ BY ITS LABEL, IN WORDS; an identifier stays monospace.
// Mutation: wrap every value in `.mono` again and the choice's label is code.
test("a choice reads in words and an identifier in monospace", () => {
  const tool = {
    sections: [
      {
        name: "GitHub",
        tool: {
          key: "github",
          configured: true,
          enabled: true,
          satisfied: true,
          form_complete: true,
          requirements: [
            req({ field: "org", label: "Organization", kind: "id", value: "acme" }),
            req({
              field: "reach",
              label: "Which activity reaches your agents",
              kind: "choice",
              value: "installed",
              choices: [
                { value: "installed", label: "Repositories where an agent's app is installed" },
              ],
            }),
          ],
        },
      },
    ],
  } as unknown as Tool;
  render(<SettingsSummary tool={tool} />);
  expect(screen.getByText("acme").className).toContain("mono");
  expect(
    screen.getByText("Repositories where an agent's app is installed").className,
  ).not.toContain("mono");
});
