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
    server: {
      deps: {
        // Vitest externalises node_modules and hands an externalised module to
        // Node's own loader, which has no idea what a stylesheet is.
        // `@crewlethq/ui`'s published entry opens with a side-effect CSS
        // import per component, so the FIRST test that renders one of them
        // fails the whole file with `Unknown file extension ".css"`. Inlined,
        // the packages go through Vite instead and the CSS is handled the way
        // the application build handles it. `src/uilet.test.tsx` is the probe
        // that keeps this rule honest.
        inline: [/@crewlethq\//],
      },
    },
  },
});
