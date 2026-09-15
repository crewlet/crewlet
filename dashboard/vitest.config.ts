import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { "~": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  test: {
    environment: "jsdom",
    globals: false,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    setupFiles: ["./src/test/setup.ts"],
    // THE SUITE RUNS IN A ZONE WITH AN OFFSET AND A DST CHANGE, because the
    // default is the machine's and every machine that runs this — CI, a
    // container, a release box — is UTC. Under UTC a whole family of date
    // bugs cannot be reproduced at all: every local midnight is an exact
    // multiple of 86_400_000 apart, so an arithmetic that truncates where it
    // should round passes, and a value bucketed in the reader's zone is
    // indistinguishable from one bucketed in the engine's.
    //
    // Berlin rather than a fixed offset: it has both halves of the hazard, a
    // non-zero offset all year and two transitions, and it is the zone
    // `internal/tracker`'s own date suite already resolves against.
    env: { TZ: "Europe/Berlin" },
  },
});
