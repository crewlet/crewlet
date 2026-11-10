// AN AGENT'S SCOPE, LAID OUT: the first entry's chip, the count of the rest
// and the control share one line inside the table at any name's length, the
// chip ending in an ellipsis rather than pushing the count or the control out.
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { LayerHost } from "@crewlethq/ui";
import "~/test/layout.tsx";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { Router } from "~/app/router.tsx";
import { AgentsAccess } from "./AgentsAccess.tsx";
import { githubAnswer } from "./fixtures.ts";

// LONGER THAN THE COLUMN at either width, so the chip has to give way.
const LONG =
  "example/customer-onboarding-notification-delivery-service-for-enterprise-tenants-in-every-region";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

async function mount(width: number) {
  await page.viewport(width, 900);
  const data = githubAnswer();
  data.options[0] = {
    ...data.options[0]!,
    scope: [LONG, "example/backend", "example/frontend"],
  };
  data.seats[0]!.lanes.github!.desired.scope = [LONG, "example/backend", "example/frontend"];
  host = document.createElement("div");
  host.style.cssText = `width: ${width}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <Router>
        <LayerHost>
          <AppAnnouncer />
          <AgentsAccess
            kind="github"
            name="GitHub"
            data={data}
            reload={() => {}}
            held={undefined}
            avatarOf={() => null}
          />
        </LayerHost>
      </Router>,
    ),
  );
  await document.fonts.ready;
}

test.each([1440, 390])(
  "a long name's chip, the count and the pencil share one line at %ipx",
  async (width) => {
    await mount(width);
    const edit = document.querySelector<HTMLElement>(
      `[aria-label="Edit QA's GitHub repositories"]`,
    )!;
    const line = edit.parentElement!;
    const chip = line.querySelector<HTMLElement>(`[title="${LONG}"]`)!;
    const count = [...line.querySelectorAll<HTMLElement>("[title]")].find((el) =>
      el.textContent?.startsWith("+2"),
    )!;
    const table = edit.closest<HTMLElement>('[role="table"]')!.getBoundingClientRect();
    const [c, n, e] = [chip, count, edit].map((el) => el.getBoundingClientRect());
    const middle = (r: DOMRect) => r.top + r.height / 2;
    expect(Math.abs(middle(c!) - middle(e!))).toBeLessThan(4);
    expect(Math.abs(middle(n!) - middle(e!))).toBeLessThan(4);
    expect(c!.right).toBeLessThanOrEqual(n!.left);
    expect(n!.right).toBeLessThanOrEqual(e!.left);
    expect(e!.right).toBeLessThanOrEqual(table.right + 0.5);
    // THE NAME IS CUT IN THE CHIP, and whole in its title.
    const label = chip.lastElementChild as HTMLElement;
    expect(label.scrollWidth).toBeGreaterThan(label.clientWidth);
  },
);
