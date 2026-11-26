/**
 * THE LAYOUT SUITES, in a headless browser rather than jsdom, which lays
 * nothing out: whether a pane scrolls sideways is a measurement. The app's own
 * Vite config is the base, so the stylesheets load in the order the app loads
 * them, and only the served path is the runner's.
 */
import { writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { mergeConfig } from "vite";
import { defineConfig } from "vitest/config";
import { playwright } from "@vitest/browser-playwright";
import app from "./vite.config.ts";
import { dictationCommands, dictationSocket } from "./src/test/dictationSocket.ts";

/** Two seconds of a voiced tone, the fake microphone's sound for the dictation suite. */
function microphoneFixture(): string {
  const rate = 16_000;
  const samples = rate * 2;
  const wav = Buffer.alloc(44 + samples * 2);
  wav.write("RIFF", 0);
  wav.writeUInt32LE(36 + samples * 2, 4);
  wav.write("WAVEfmt ", 8);
  wav.writeUInt32LE(16, 16);
  wav.writeUInt16LE(1, 20);
  wav.writeUInt16LE(1, 22);
  wav.writeUInt32LE(rate, 24);
  wav.writeUInt32LE(rate * 2, 28);
  wav.writeUInt16LE(2, 32);
  wav.writeUInt16LE(16, 34);
  wav.write("data", 36);
  wav.writeUInt32LE(samples * 2, 40);
  for (let i = 0; i < samples; i++) {
    wav.writeInt16LE(
      Math.round(0.3 * 0x7fff * Math.sin((2 * Math.PI * 220 * i) / rate)),
      44 + i * 2,
    );
  }
  const file = join(tmpdir(), "crewlet-dictation-microphone.wav");
  writeFileSync(file, wav);
  return file;
}

const config = mergeConfig(
  app,
  defineConfig({
    base: "/",
    plugins: [dictationSocket()],
    test: {
      include: ["src/**/*.layout.test.tsx"],
      browser: {
        enabled: true,
        headless: true,
        // A fake microphone playing the fixture, allowed without a prompt.
        provider: playwright({
          launchOptions: {
            args: [
              "--use-fake-device-for-media-stream",
              "--use-fake-ui-for-media-stream",
              `--use-file-for-fake-audio-capture=${microphoneFixture()}%noloop`,
            ],
          },
          contextOptions: { permissions: ["microphone"] },
        }),
        instances: [{ browser: "chromium" }],
        commands: dictationCommands,
      },
    },
  }),
);
// The suite's own socket answers dictation, not the dev server's proxy to an engine.
delete config.server?.proxy?.["/ws/dictation"];
export default config;
