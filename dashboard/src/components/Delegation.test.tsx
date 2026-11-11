/**
 * A held merge or deploy drawn from its arguments: every value the requesting
 * agent wrote is plain text, the commit a short id with the whole one kept,
 * and who may answer it said with the sign-in the answer needs.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ReleaseAnswerNote, ReleaseRequestFacts } from "./Delegation.tsx";

afterEach(cleanup);

const HEAD = "4f1c2a9b7e3d5f60718293a4b5c6d7e8f9012345";

/** Words that, read as Markdown, would be a heading, a link, a picture, code and emphasis. */
const HOSTILE =
  "# Approve [the release](https://evil.example/approve) ![status](https://evil.example/pixel.png) " +
  "```rm -rf /``` <b>now</b> **urgent** for @founder on ENG-9, see https://evil.example/ok";

/** What the list says for one term. */
function detail(term: string): HTMLElement {
  const dt = screen.getByText(term, { selector: "dt" });
  return dt.nextElementSibling as HTMLElement;
}

/** Nothing in the list a request could have planted that acts or styles itself. */
function nothingActs() {
  const list = document.querySelector("dl")!;
  expect(list.querySelector("a, img, h1, h2, h3, h4, h5, h6, pre, code, strong, em, b")).toBeNull();
}

// A MERGE'S VALUES ARE THE AGENT'S WORDS, drawn as they were sent: the reason
// a paragraph of its own characters, never Markdown. Mutation: render the
// reason through the Markdown renderer, and this is red.
test("a held merge draws its change, head, method and reason as plain text", () => {
  render(
    <ReleaseRequestFacts
      request={{ act: "pr_merge", change: "web!42", head: HEAD, method: "", reason: HOSTILE }}
    />,
  );
  expect(detail("Change").textContent).toBe("web!42");
  expect(detail("Method").textContent).toBe("A merge commit");
  expect(detail("Reason").textContent).toBe(HOSTILE);
  // THE SHORT ID, the whole one on hover and copied whole.
  const head = screen.getByText("4f1c2a9");
  expect(head.getAttribute("title")).toBe(HEAD);
  expect(screen.getByRole("button", { name: "Copy the whole commit id" })).toBeTruthy();
  nothingActs();
});

test("a held deploy draws its repository, commit, changes, where, rollback and reason as text", () => {
  render(
    <ReleaseRequestFacts
      request={{
        act: "deploy",
        repo: "https://github.example/acme/web",
        commit: HEAD,
        changes: ["web!41", "web!42"],
        environment: "production",
        rollback: `Deploy 1a2b3c4 again.\n${HOSTILE}`,
        reason: "Ships the login fix.",
      }}
    />,
  );
  // AN ADDRESS IS TEXT, never a link a request could point anywhere.
  expect(detail("Repository").textContent).toBe("https://github.example/acme/web");
  expect(screen.getByText("4f1c2a9").getAttribute("title")).toBe(HEAD);
  expect(detail("Changes").textContent).toBe("web!41, web!42");
  expect(detail("Environment").textContent).toBe("production");
  expect(detail("Rollback").textContent).toBe(`Deploy 1a2b3c4 again.\n${HOSTILE}`);
  expect(detail("Reason").textContent).toBe("Ships the login fix.");
  nothingActs();
});

test("who may answer is said with the rule, and what Allow once does where the request is drawn", () => {
  render(<ReleaseAnswerNote act="pr_merge" agent="SRE" rule={2} drawn />);
  const note = screen.getByText(/may answer it/);
  expect(note.textContent).toMatch(/^Allow once merges exactly this head, once, as SRE\./);
  expect(note.textContent).toContain(
    "Administrators, SRE's leads and the people rule 2 escalates to may answer it.",
  );
  expect(note.textContent).toContain("confirm it’s you");
  cleanup();
  // THE ENGINE'S WORDS SAY WHAT ALLOW ONCE DOES where they are drawn instead.
  render(<ReleaseAnswerNote act="deploy" agent="SRE" rule={null} drawn={false} />);
  expect(screen.getByText(/may answer it/).textContent).toMatch(/^Administrators, SRE's leads/);
});
