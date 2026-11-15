/**
 * THE SECURITY CARD'S LINES, LAID OUT: every gesture stands beside the line it
 * changes, at a phone's width as at a desktop's, whatever the line says.
 *
 * A wrapping row sized the words to their one line, so whether the button
 * dropped under them turned on the sentence's length: at 390 "Change
 * password…" fell under its line while "Set up…" stayed beside its shorter
 * one. Mutation: lay the line out as a wrapping row again and the long line's
 * button is under its words.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Button } from "@crewlethq/ui";
import "~/test/layout.tsx";
import { SecurityLine } from "./Account.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

const LINES = [
  { label: "Two-step verification", words: "Off. Password only.", action: "Set up…" },
  {
    label: "Recovery codes",
    words: "None yet. Create a set for when you don’t have your app.",
    action: "Create new codes…",
  },
  { label: "Password", words: "Last set Nov 14, 2026.", action: "Change password…" },
];

/**
 * The lines whose button is not beside its words: under them, over them, or
 * past the card. Beside is measured against the words' whole column (the name
 * and its sentence), whatever lines the sentence wraps to.
 */
function dropped(): string[] {
  const out: string[] = [];
  for (const line of LINES) {
    const section = document.querySelector<HTMLElement>(`[aria-label="${line.label}"]`)!;
    const button = [...section.querySelectorAll("button")].find(
      (b) => b.textContent === line.action,
    )!;
    const words = section.firstElementChild!.getBoundingClientRect();
    const b = button.getBoundingClientRect();
    const box = section.getBoundingClientRect();
    if (b.top >= words.bottom - 0.5) out.push(`${line.action}: under its line`);
    if (b.left < words.right - 0.5) out.push(`${line.action}: over its line`);
    if (b.right > box.right + 0.5) out.push(`${line.action}: past the card`);
  }
  return out;
}

// THE CARD'S INNER WIDTH at a 390 phone, and at a desktop's column.
test.each([326, 720])("every line keeps its gesture beside it at %ipx", async (width) => {
  await page.viewport(390, 900);
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <>
        {LINES.map((line) => (
          <SecurityLine
            key={line.label}
            label={line.label}
            action={
              <Button size="small" variant="secondary">
                {line.action}
              </Button>
            }
          >
            {line.words}
          </SecurityLine>
        ))}
      </>,
    ),
  );
  await document.fonts.ready;
  expect(dropped()).toEqual([]);
});
