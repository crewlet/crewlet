import { expect, test } from "vitest";
import { pageName, taskBody, taskTitle } from "./brief.ts";

// THE TASK IS SHOWN EXACTLY AS IT IS FILED, so what it says is read here.
test("the task names the page by its title, else the brief's first words", () => {
  expect(taskTitle({ title: " Venue poll ", brief: "x", capabilities: [] })).toBe(
    "Build an HTML page: Venue poll",
  );
  expect(pageName({ title: "", brief: "A poll for the offsite\nmore", capabilities: [] })).toBe(
    "A poll for the offsite",
  );
  const long =
    "A very long first line that keeps going well past the sixty characters a name takes";
  expect(pageName({ title: "", brief: long, capabilities: [] })).toBe(
    "A very long first line that keeps going well past the sixty…",
  );
  expect(taskTitle({ title: "", brief: "  ", capabilities: [] })).toBe("Build an HTML page");
});

test("the body carries the brief, the capabilities in both words and how it is delivered", () => {
  const body = taskBody({
    title: "Venue poll",
    brief: "A poll.\n",
    capabilities: ["identity", "db"],
  });
  expect(body).toBe(
    [
      "A poll.",
      "What the page can do:\n- Saves what people enter (the `db` capability)\n- Knows who is using it (the `identity` capability)",
      "Title it “Venue poll”, declare only the capabilities above, and share it with me as an editor.",
    ].join("\n\n"),
  );
  expect(taskBody({ title: "", brief: "A calculator.", capabilities: [] })).toContain(
    "The page needs no capabilities",
  );
});
