/**
 * The engine's sentences as a notice draws them, and the warnings as one
 * quiet callout: a standing banner rests as one line, an answer rests open.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { ConfigWarning } from "~/protocol/types.ts";
import { ConfigWarnings, engineWords } from "./ConfigWarnings.tsx";

afterEach(() => {
  cleanup();
  localStorage.clear();
});

const drawn = (text: string) => render(<p>{engineWords(text)}</p>).container.firstElementChild!;

test("a sentence opening on a word is raised, and a backticked key is code", () => {
  const p = drawn("this company has no `kind: human` seats, so nobody can be invited");
  expect(p.textContent).toBe("This company has no kind: human seats, so nobody can be invited");
  expect(p.querySelector("code")?.textContent).toBe("kind: human");
});

test("a message opening on a field path keeps the key's case", () => {
  expect(drawn("units[0].roles[0].goal: missing").textContent).toBe(
    "units[0].roles[0].goal: missing",
  );
});

const warning = (seat: string, item = ""): ConfigWarning => ({
  kind: "advisory",
  ref: "",
  path: "roles[0].schedules[0]",
  segments: null,
  seat,
  unit: "",
  from: "",
  to: "",
  message: `${seat} runs a schedule often`,
  class: "schedule.frequent",
  title: "Schedules run 12 or more times a day",
  person: "Each run starts a turn whether or not anything changed.",
  item,
});
const seat = (handle: string) => ({ name: handle.toUpperCase(), agent: true });
const WARNINGS = [
  warning("sre", "health-check"),
  warning("sre", "release-window"),
  warning("support", "inbox-sweep"),
];

test("a standing banner rests closed as one line, and remembers being opened", () => {
  const banner = () =>
    render(
      <ConfigWarnings
        warnings={WARNINGS}
        seat={seat}
        surface="test"
        label="Warnings about the configuration"
      />,
    );
  banner();
  const group = screen.getByRole("group", { name: "Warnings about the configuration" });
  expect(group.className).toContain("crewlet-callout--quiet");
  const toggle = within(group).getByRole("button", { name: "1 configuration warning Review" });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  expect(within(group).queryByRole("listitem")).toBeNull();
  fireEvent.click(toggle);
  expect(localStorage.getItem(`${STORAGE_KEYS.warningsOpen}test`)).toBe("open");
  cleanup();
  banner();
  expect(
    screen
      .getByRole("button", { name: "1 configuration warning Hide" })
      .getAttribute("aria-expanded"),
  ).toBe("true");
});

test("each kind is one line naming what it applies to, its specifics and explanation behind it", () => {
  render(<ConfigWarnings warnings={WARNINGS} seat={seat} />);
  const row = screen.getByRole("button", { name: "Schedules run 12 or more times a day 2 agents" });
  expect(row.getAttribute("aria-expanded")).toBe("false");
  const panel = document.getElementById(row.getAttribute("aria-controls")!)!;
  expect(panel.hidden).toBe(true);
  fireEvent.click(row);
  expect([...panel.querySelectorAll("p")].map((p) => p.textContent)).toEqual([
    "SRE: health-check, release-window",
    "SUPPORT: inbox-sweep",
    "Each run starts a turn whether or not anything changed.",
  ]);
});
