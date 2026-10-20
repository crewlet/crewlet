/**
 * The variant bar says which variant and which scenario are on screen, and
 * every press on it reaches the page: another variant from its chip or from
 * `[` and `]`, another scenario from the menu its trigger names, and Reset,
 * which starts the scenario on screen over rather than choosing one.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi, type Mock } from "vitest";
import { resumeAll, scenario } from "../fixtures/scenarios.ts";
import { defineTopic, LABEL_MAX, topicProblems, type Topic } from "../harness/defineTopic.ts";
import { VariantBar } from "../harness/VariantBar.tsx";

const topic = defineTopic({
  id: "bar-check",
  title: "Variant bar check",
  start: "#/home",
  scenarios: [
    scenario("today", "As today", (c) => c),
    scenario("busy", "Both agents working", resumeAll),
  ],
  variants: [
    { id: "today", label: "Today", summary: "The dashboard as it ships." },
    ...["a", "b", "c", "d", "e"].map((id, n) => ({
      id,
      label: `${id.toUpperCase()} · One list`,
      summary: `Proposal ${n + 1}, in one line.`,
    })),
  ],
});

let onScenario: Mock<(id: string) => void>;
let onReset: Mock<() => void>;
let onVariant: Mock<(id: string) => void>;

function bar(variant = "today", on = "busy", of: Topic = topic) {
  return render(
    <VariantBar
      topic={of}
      variant={of.variants.find((v) => v.id === variant)!}
      scenario={on}
      onScenario={onScenario}
      onReset={onReset}
      onVariant={onVariant}
    />,
  );
}

beforeEach(() => {
  onScenario = vi.fn<(id: string) => void>();
  onReset = vi.fn<() => void>();
  onVariant = vi.fn<(id: string) => void>();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

describe("the scenario", () => {
  test("on screen is named on its trigger, whose menu offers the others", () => {
    bar();
    const trigger = screen.getByRole("button", { name: "Scenario: Both agents working" });
    expect(trigger.getAttribute("aria-haspopup")).toBe("menu");
    fireEvent.click(trigger);
    const items = within(screen.getByRole("menu")).getAllByRole("menuitemradio");
    expect(items.map((i) => [i.textContent, i.getAttribute("aria-checked")])).toEqual([
      ["As today", "false"],
      ["Both agents working", "true"],
    ]);
    fireEvent.click(items[0]!);
    expect(onScenario).toHaveBeenCalledWith("today");
  });

  test("is started over by Reset, which reports a reset and never chooses a scenario", () => {
    bar();
    fireEvent.click(screen.getByRole("button", { name: "Reset the demo" }));
    expect(onReset).toHaveBeenCalledTimes(1);
    expect(onScenario).not.toHaveBeenCalled();
  });
});

describe("the variants", () => {
  test("are one chip each, the one on screen checked, and a press opens another", () => {
    bar("c");
    const chips = within(screen.getByRole("radiogroup", { name: "Variant" })).getAllByRole("radio");
    expect(chips.map((c) => c.textContent)).toEqual([
      "Today",
      "A · One list",
      "B · One list",
      "C · One list",
      "D · One list",
      "E · One list",
    ]);
    expect(chips[3]?.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(chips[1]!);
    expect(onVariant).toHaveBeenCalledWith("a");
  });

  test("step with ] and [, round the ends, unless the page took the press", async () => {
    bar("e");
    const press = async (key: string, taken = false) => {
      const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
      if (taken) event.preventDefault();
      window.dispatchEvent(event);
      await new Promise((r) => setTimeout(r, 0));
    };
    await press("]");
    expect(onVariant).toHaveBeenLastCalledWith("today");
    await press("[");
    expect(onVariant).toHaveBeenLastCalledWith("d");
    await press("]", true);
    expect(onVariant).toHaveBeenCalledTimes(2);
  });
});

describe("folding", () => {
  test("leaves a pill naming the variant, which opens the bar again", () => {
    bar("b");
    fireEvent.click(screen.getByRole("button", { name: "Fold the variant bar" }));
    expect(screen.queryByRole("radiogroup")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "B · One list" }));
    expect(screen.getByRole("radiogroup", { name: "Variant" })).toBeTruthy();
  });
});

describe("a label", () => {
  test("longer than one control holds is refused when the topic is declared", () => {
    const long = "x".repeat(LABEL_MAX + 1);
    const problems = topicProblems({
      ...topic,
      scenarios: [scenario("today", long, (c) => c)],
      variants: [...topic.variants, { id: "f", label: long, summary: "Too long a name." }],
    });
    expect(problems).toEqual([
      `scenario "today"'s label is longer than ${LABEL_MAX} characters`,
      `variant "f"'s label is longer than ${LABEL_MAX} characters`,
    ]);
  });
});
