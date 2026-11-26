/**
 * DICTATION END TO END IN A REAL BROWSER: Chromium's fake microphone plays a
 * voiced fixture, the kit opens it, the relay's audio worklet turns it into
 * 16 kHz PCM16 frames, and the socket carries them to the test server's
 * `/ws/dictation`, which answers with the engine's protocol and its fake's
 * rule for words. Press, partials, Accept, Cancel, preparing, busy and unmount.
 */
import "~/test/layout.tsx";
import { useState } from "react";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeAll, expect, test } from "vitest";
import { eventually } from "~/test/settle.ts";
import { commands } from "vitest/browser";
import { Announcer, Composer, preloadDictation, type DictationSource } from "@crewlethq/ui";
import { openDictationSocket } from "~/protocol/dictation.ts";
import { pcmCapture } from "./audio.ts";
import { createRelay } from "./relay.ts";
import type { DictationMode, DictationRecord } from "~/test/dictationSocket.ts";

declare module "vitest/browser" {
  interface BrowserCommands {
    dictationMode: (mode: DictationMode) => Promise<void>;
    dictationRecords: () => Promise<DictationRecord[]>;
  }
}

const source: DictationSource = {
  transport: createRelay({
    available: () => true,
    socket: openDictationSocket,
    capture: pcmCapture,
  }),
};

let root: Root | null = null;
let host: HTMLElement | null = null;
let committed: string[] = [];

function Box() {
  const [value, setValue] = useState("Draft:");
  return (
    <Composer
      label="Reply"
      value={value}
      onChange={(next) => {
        committed.push(next);
        setValue(next);
      }}
      dictation={source}
      actions={<button type="button">Send</button>}
    />
  );
}

function mount() {
  host = document.createElement("div");
  host.style.width = "720px";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <>
        <Announcer />
        <Box />
      </>,
    ),
  );
}

const field = () => host!.querySelector<HTMLTextAreaElement>("textarea")!;
const press = (name: string) =>
  host!.querySelector<HTMLButtonElement>(`button[aria-label="${name}"]`)!.click();
const shown = () => host!.querySelector(".crewlet-dictation__provisional")?.textContent ?? "";
const note = () => document.querySelector(".crewlet-dictation__note")?.textContent ?? "";
const last = async () => (await commands.dictationRecords()).at(-1)!;

beforeAll(async () => {
  await preloadDictation();
});

afterEach(async () => {
  root?.unmount();
  host?.remove();
  root = null;
  host = null;
  committed = [];
  await commands.dictationMode("ready");
});

test("a press streams 40 ms frames of 16 kHz PCM16, partials show at the caret, and Enter accepts once", async () => {
  mount();
  press("Dictate");
  await eventually(() => expect(shown()).toMatch(/Ship the/));
  const sent = await last();
  expect(sent.start).toEqual({ type: "start", lang: "en", rate: 16000, encoding: "pcm16" });
  expect(sent.sizes).toEqual([1280]);
  expect(field().value).toBe("Draft:");
  field().dispatchEvent(
    new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }),
  );
  await eventually(() => expect(field().value).toMatch(/^Draft: Ship the/));
  expect(committed).toHaveLength(1);
  expect((await last()).controls).toEqual(["finish"]);
  await eventually(async () => expect((await last()).closed).toBe(true));
});

test("Escape cancels: the draft is untouched and the engine is told", async () => {
  mount();
  press("Dictate");
  await eventually(() => expect(shown()).toMatch(/Ship/));
  field().dispatchEvent(
    new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }),
  );
  await eventually(async () => expect((await last()).controls).toEqual(["cancel"]));
  expect(field().value).toBe("Draft:");
  expect(committed).toEqual([]);
});

test("preparing says so with no words, then listens once ready", async () => {
  await commands.dictationMode("preparing");
  mount();
  press("Dictate");
  await eventually(() => expect(note()).toMatch(/Preparing dictation/));
  expect(shown()).toBe("");
  await eventually(() => expect(shown()).toMatch(/Ship/));
});

test("busy before the first word says so and gives the mic back", async () => {
  await commands.dictationMode("busy");
  mount();
  press("Dictate");
  await eventually(() =>
    expect(note()).toBe("Dictation is busy right now. Try again in a moment."),
  );
  await eventually(() =>
    expect(host!.querySelector('button[aria-label="Dictate"]')).not.toBeNull(),
  );
  expect(committed).toEqual([]);
});

test("unmounting mid-dictation closes the socket and writes nothing", async () => {
  mount();
  press("Dictate");
  await eventually(() => expect(shown()).toMatch(/Ship/));
  root!.unmount();
  root = null;
  await eventually(async () => expect((await last()).closed).toBe(true));
  expect(committed).toEqual([]);
});
