/**
 * The engine's refusal, rendered as the list of problems it is.
 *
 * A validation refusal is several of them joined with newlines, each one
 * `path: kind: what to do`. HTML collapses those newlines, so the banner
 * showed one run-on paragraph in which the second problem's config path ran
 * into the end of the first one's sentence.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { Problems, marked, split } from "./Problems.tsx";

afterEach(cleanup);

// The two the engine refuses a half-filled Atlassian form with, verbatim.
const refusal =
  "integrations.confluence: required value missing: give url (a Data Center " +
  "instance or a Cloud site) or cloud_id (an Atlassian Cloud id): without one " +
  "there is nowhere to search\n" +
  "integrations.confluence.webhook_secret: required value missing: required " +
  "for a Data Center instance: the /webhooks/confluence route has nothing to " +
  "verify a delivery with otherwise, and answers 503 to every one";

// TWO PROBLEMS ARE TWO LINES. Joined, the second one's path read as the end
// of the first one's sentence: "…nowhere to search
// integrations.confluence.webhook_secret: required value missing…".
test("a refusal is split into the problems it holds", () => {
  const problems = split(refusal);
  expect(problems.length).toBe(2);
  expect(problems[0]?.path).toBe("integrations.confluence");
  expect(problems[1]?.path).toBe("integrations.confluence.webhook_secret");
  // THE ENGINE'S OWN WORDS, unaltered: a message this screen rewrote would
  // be a second, quieter statement of a rule that lives in Go.
  expect(problems[0]?.text).toContain("nowhere to search");
});

// A SENTENCE WITHOUT A PATH IS LEFT WHOLE. Not every refusal names a place in
// the document, and inventing a head for one would put a fragment of prose in
// the face reserved for config paths.
test("a refusal that names no path is one plain line", () => {
  const problems = split("The engine refused that.");
  expect(problems).toEqual([{ text: "The engine refused that." }]);
});

// EACH KIND OF THING GETS ITS OWN FACE, because a wall of one makes a reader
// parse the punctuation to work out which is which.
test("values, routes, references and links are picked out of a sentence", () => {
  render(
    <span>
      {marked(
        'set ${ATLASSIAN_ORG_ID}, the /webhooks/confluence route, "auto", ' +
          "integrations.github.provisioning.org, or see https://example.com/docs",
      )}
    </span>,
  );

  // A SEALED ENTRY wears the face it wears in a field and on the Secrets
  // screen: it names something in the store, not a path in the document.
  const ref = screen.getByText("${ATLASSIAN_ORG_ID}");
  expect(ref.tagName).toBe("CODE");
  expect(ref.className).toContain("is-reference");

  expect(screen.getByText("/webhooks/confluence").tagName).toBe("CODE");
  expect(screen.getByText('"auto"').tagName).toBe("CODE");
  expect(screen.getByText("integrations.github.provisioning.org").tagName).toBe("CODE");

  // A LINK IS SOMEWHERE TO GO, and it opens in a new tab: this sits in a
  // dialog holding a half-filled form, and following it in place would throw
  // the form away to read a page about how to fill it in.
  const link = screen.getByRole("link", { name: "https://example.com/docs" });
  expect(link.getAttribute("href")).toBe("https://example.com/docs");
  expect(link.getAttribute("target")).toBe("_blank");
});

// ONE PROBLEM IS A SENTENCE, not a list of one: a bullet on its own reads as
// the first of several and sets a reader looking for the rest.
test("a single problem is not drawn as a list", () => {
  const { container } = render(<Problems detail="integrations.jira: required value missing" />);
  expect(container.querySelector("ul")).toBeNull();
  expect(screen.getByText("integrations.jira").tagName).toBe("CODE");
});

// AND SEVERAL ARE, one per line.
test("several problems are one line each", () => {
  const { container } = render(<Problems detail={refusal} />);
  expect(container.querySelectorAll("li").length).toBe(2);
  // The route inside the second one is still picked out.
  expect(screen.getByText("/webhooks/confluence").tagName).toBe("CODE");
});
