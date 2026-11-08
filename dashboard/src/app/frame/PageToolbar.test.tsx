import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { PageToolbar } from "./PageToolbar.tsx";

afterEach(cleanup);

const SRC = join(__dirname, "..", "..");

/** Every screen module, tests left out. */
function sources(dir = join(SRC, "routes")): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return sources(path);
    return /\.tsx$/.test(name) && !/\.test\.tsx$/.test(name) ? [path] : [];
  });
}

/** Each `<PageToolbar>…</PageToolbar>` in a module, as written. */
function bars(text: string): string[] {
  const out: string[] = [];
  for (
    let at = text.indexOf("<PageToolbar");
    at !== -1;
    at = text.indexOf("<PageToolbar", at + 1)
  ) {
    out.push(text.slice(at, text.indexOf("</PageToolbar>", at)));
  }
  return out;
}

/** The attributes a JSX tag is written with, up to the `>` that closes it. */
function attributes(text: string, from: number): string {
  let depth = 0;
  // A type argument (`<SegmentedControl<Shape>`) is not the tag's end.
  const generic = /^<\w+<[^>]*>/.exec(text.slice(from));
  for (let i = from + (generic ? generic[0].length : 0); i < text.length; i++) {
    const c = text[i];
    if (c === "{") depth++;
    else if (c === "}") depth--;
    else if (c === ">" && depth === 0) return text.slice(from, i);
  }
  return text.slice(from);
}

// A WRAPPED BAR IS TALLER THAN ONE BAND, and only the Work list published its
// drawn height: a History or Audit bar wrapped on a phone, and the grid's
// column heads stuck under the one-band offset, behind its second line.
test("a screen's toolbar is a sticky band that publishes its drawn height", () => {
  const height = vi
    .spyOn(HTMLElement.prototype, "getBoundingClientRect")
    .mockReturnValue({ height: 96 } as DOMRect);
  try {
    const { container } = render(
      <main className="crewlet-app-shell__main">
        <PageToolbar className="work-bar" aria-label="Filters" />
      </main>,
    );
    const bar = container.querySelector(".toolbar.work-bar")!;
    expect(bar.getAttribute("aria-label")).toBe("Filters");
    const scroller = container.querySelector<HTMLElement>(".crewlet-app-shell__main")!;
    expect(scroller.style.getPropertyValue("--toolbar-band")).toBe("96px");
  } finally {
    height.mockRestore();
  }
});

test("every screen's toolbar is a PageToolbar, so every one publishes its band", () => {
  const raw = sources()
    .filter((path) => /className=(["'`{])\s*`?toolbar(\s|["'`])/.test(readFileSync(path, "utf8")))
    .map((path) => relative(SRC, path));
  expect(raw).toEqual([]);
});

// ONE HEIGHT STEP A ROW. The Work list's Display button stood 26px beside a
// 30px search box, and History's window 28px beside 26px pickers: every
// control written in a toolbar row takes the small step.
test("every control written in a toolbar row stands at the small step", () => {
  const STEP: Record<string, RegExp> = {
    Select: /\bsize="sm"/,
    SegmentedControl: /\bsize="sm"/,
    TimeRangePicker: /\bsize="sm"/,
    RepoPicker: /\bsize="sm"/,
    IconButton: /\bsize="sm"/,
    Input: /\binputSize="sm"/,
    Button: /\bsize="small"/,
  };
  const off: string[] = [];
  let seen = 0;
  for (const path of sources()) {
    for (const bar of bars(readFileSync(path, "utf8"))) {
      for (const m of bar.matchAll(
        /<(Select|SegmentedControl|TimeRangePicker|RepoPicker|IconButton|Input|Button)\b/g,
      )) {
        seen++;
        const written = attributes(bar, m.index!);
        if (!STEP[m[1]!]!.test(written))
          off.push(`${relative(SRC, path)}: <${m[1]}${written.slice(m[1]!.length + 1, 80)}`);
      }
    }
  }
  expect(seen).toBeGreaterThan(20);
  expect(off).toEqual([]);
});
