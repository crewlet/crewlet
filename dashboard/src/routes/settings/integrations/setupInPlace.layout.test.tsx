// THE SETTINGS FORM IN PLACE, LAID OUT inside an integration's Settings tab at
// 1440 and 390px in light and dark: its Setup steps control ends at the tab
// panel's own end edge (where Check now and the tab rule end), and nothing in
// the form runs past it.
import { afterEach, describe, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { overflowing } from "~/test/layout.tsx";
import type { SetupToolState } from "~/protocol/index.ts";
import { SetupForm } from "../SetupForm.tsx";
// THE PAGE'S OWN STYLES, which the page loads through its parts.
import "~/styles/integrations.css";

/** Google Cloud half set up: its provisioner saved, its provider still to paste. */
const GOOGLE_CLOUD = {
  key: "google_cloud",
  configured: true,
  enabled: true,
  satisfied: false,
  form_complete: false,
  requirements: [
    {
      field: "provisioner",
      label: "Provisioner service account",
      kind: "id",
      config_path: "integrations.google_cloud.provisioning.provisioner",
      required: true,
      connect: true,
      present: true,
      resolved: true,
      value: "crewlet-provisioner@acme-ids.iam.gserviceaccount.com",
    },
    {
      field: "provider",
      label: "Workload identity provider",
      kind: "id",
      config_path: "integrations.google_cloud.provisioning.provider",
      required: true,
      connect: true,
      present: false,
      resolved: null,
    },
  ],
  prepare: [
    { text: [{ text: "Create an empty project for Crewlet's service accounts." }] },
    { text: [{ text: "Enter the provisioner's address here and save." }], yields: "provisioner" },
    { text: [{ text: "Run the block Crewlet shows in Cloud Shell." }] },
    { text: [{ text: "Paste the last line the block prints." }], yields: "provider" },
  ],
} as unknown as SetupToolState;

/** The Settings tab's panel width beside the directory at each page width. */
const PANEL = { 1440: 840, 390: 358 } as const;

let host: HTMLElement | null = null;
let root: Root | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

async function mount(width: 1440 | 390, theme: "light" | "dark") {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  host = document.createElement("div");
  host.style.cssText = `width: ${PANEL[width]}px;`;
  host.className = "int-tab";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <ToastProvider>
        <Router>
          <SetupForm
            sections={[{ name: "Google Cloud", tool: GOOGLE_CLOUD }]}
            title="Google Cloud"
            onClose={() => {}}
            onDone={() => {}}
          />
        </Router>
      </ToastProvider>,
    ),
  );
  await document.fonts.ready;
}

describe.each(["light", "dark"] as const)("on a %s document", (theme) => {
  // Mutation: give the form back part of the pane's end inset again and the
  // control ends past the panel's edge.
  test.each([1440, 390] as const)(
    "at %ipx Setup steps ends at the panel's edge, and nothing runs past it",
    async (width) => {
      await mount(width, theme);
      const toggle = [...host!.querySelectorAll("button")].find((b) =>
        b.textContent?.includes("Setup steps"),
      );
      expect(toggle).toBeDefined();
      const edge = host!.getBoundingClientRect().right;
      expect(Math.round(toggle!.getBoundingClientRect().right - edge), `width ${width}`).toBe(0);
      expect(overflowing(host!)).toEqual([]);
    },
  );
});
