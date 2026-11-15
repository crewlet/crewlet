/**
 * A HOST LOGIN, LAID OUT: its lines take the block's width and its one action,
 * "Check again", keeps its own, as every other button in the dashboard does.
 * Stretched with the lines, it was a bar the width of the wizard's form.
 */
import "~/styles/models.css";
import { afterEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { HostLogin } from "./HostLogin.tsx";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
});

test("Check again keeps its own width beside the lines it checks", async () => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  await page.viewport(1440, 900);
  host = document.createElement("div");
  host.style.cssText = "width: 640px;";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={{ name: "Acme", timezone: "UTC", roles: [], units: [] }}>
        <HostLogin
          modelKey="default"
          logins={[
            {
              node: "node-a",
              state: "signed_out",
              method: null,
              account: null,
              checked_at: null,
              command: "crewlet llm login default",
            },
          ]}
          onChecked={() => {}}
        />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  const block = host.querySelector<HTMLElement>(".models-host")!;
  const button = [...block.querySelectorAll("button")].find(
    (b) => b.textContent === "Check again",
  )!;
  const lines = block.querySelector<HTMLElement>(".models-host__nodes")!;
  // THE LINES TAKE THE BLOCK'S WIDTH; THE BUTTON ITS LABEL'S.
  expect(Math.round(lines.getBoundingClientRect().width)).toBe(
    Math.round(block.getBoundingClientRect().width),
  );
  expect(button.getBoundingClientRect().width).toBeLessThan(
    block.getBoundingClientRect().width / 3,
  );
});
