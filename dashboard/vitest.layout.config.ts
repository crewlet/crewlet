/**
 * THE LAYOUT SUITES, in a headless browser rather than jsdom, which lays
 * nothing out: whether a pane scrolls sideways is a measurement. The app's own
 * Vite config is the base, so the stylesheets load in the order the app loads
 * them, and only the served path is the runner's.
 */
import { mergeConfig } from "vite";
import { defineConfig } from "vitest/config";
import { playwright } from "@vitest/browser-playwright";
import app from "./vite.config.ts";

export default mergeConfig(
  app,
  defineConfig({
    base: "/",
    test: {
      include: ["src/**/*.layout.test.tsx"],
      browser: {
        enabled: true,
        headless: true,
        provider: playwright(),
        instances: [{ browser: "chromium" }],
      },
    },
  }),
);
