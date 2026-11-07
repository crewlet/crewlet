import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { RunQuestionNotice } from "./RunQuestion.tsx";
import { Router } from "~/app/router.tsx";

vi.mock("~/lib/linkHosts.ts", () => ({
  useLinkHosts: () => [{ kind: "github", url: "https://github.com", owners: ["acme"] }],
}));

afterEach(cleanup);

const draw = (question: string | undefined) =>
  render(
    <Router>
      <RunQuestionNotice question={question} empty="(no question recorded)" />
    </Router>,
  );

const LONG = `${"Should the client retry past the ceiling the API documents? ".repeat(6)}Which one?`;

// A NOTICE HAS NO READING BEHIND IT BUT ITS ANSWER, which is a write: a long
// question is clamped with a way to read every word before answering, and the
// whole of it stays in the page for a screen reader.
test("a long question is clamped with a way to read all of it", () => {
  const { container } = draw(LONG);
  const words = container.querySelector(".crewlet-text") as HTMLElement;
  expect(words.textContent).toContain("Which one?");
  expect(words.className).toContain("clamp-3");
  const more = screen.getByRole("button", { name: "Show all" });
  expect(more.getAttribute("aria-expanded")).toBe("false");
  expect(more.getAttribute("aria-controls")).toBe(words.id);
  fireEvent.click(more);
  expect(words.className).not.toContain("clamp");
  expect(screen.getByRole("button", { name: "Show less" }).getAttribute("aria-expanded")).toBe(
    "true",
  );
});

test("a question within the title's cap is drawn whole, with nothing to open", () => {
  draw("Which retry ceiling should the client use?");
  expect(screen.getByText("Which retry ceiling should the client use?")).toBeTruthy();
  expect(screen.queryByRole("button")).toBeNull();
});

test("a run with no question says so", () => {
  draw("  ");
  expect(screen.getByText("(no question recorded)")).toBeTruthy();
});

// AN ADDRESS ON THE COMPANY'S OWN HOST IS DRAWN AS WHAT IT NAMES AND OPENS, so a
// pull request's address no longer uses up the preview. A model wrote these
// words, so any other address is no control: a click must not hand what the
// model read to whoever owns the host, and it stays whole so a reader sees
// exactly where it points.
test("a question's own address is a short link, and any other opens nothing", () => {
  const { container } = draw(
    "Is the fix in https://github.com/acme/frontend/pull/14 or https://elsewhere.example.net/?q=secret right?",
  );
  const own = container.querySelector("a") as HTMLAnchorElement;
  expect(own.getAttribute("href")).toBe("https://github.com/acme/frontend/pull/14");
  expect(own.textContent).toContain("acme/frontend#14");
  expect(container.textContent).not.toContain("https://github.com/acme/frontend/pull/14");
  expect(container.querySelector('a[href*="elsewhere.example.net"]')).toBeNull();
  expect(container.textContent).toContain("elsewhere.example.net");
});
