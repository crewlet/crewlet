/**
 * Dictation for every composer: the engine relay first, the browser's
 * on-device recognition after it, and the mic held with the engine's reason
 * where neither can work. Only the button is in the entry; the relay and its
 * worklet load on the first press.
 */
import { useEffect, useMemo, useState } from "react";
import {
  webSpeechLocalTransport,
  type DictationSource,
  type DictationTransport,
} from "@crewlethq/ui";
import type { DictationReason } from "~/contract/dictation.ts";
import type { ViewerDictation } from "~/protocol/index.ts";
import { canOpenDictationSocket, openDictationSocket } from "~/protocol/dictation.ts";
import { useViewer } from "~/lib/viewer.ts";

/** The engine's last word on dictation, read by the relay at the press. */
let engine: ViewerDictation | undefined;

/** Whether this browser can run the relay at all: a secure page with an audio worklet and a socket. */
function relaySupported(): boolean {
  return (
    typeof window !== "undefined" &&
    window.isSecureContext &&
    typeof AudioWorkletNode !== "undefined" &&
    canOpenDictationSocket()
  );
}

/** The relay route, its code fetched on the first press. */
function relayTransport(): DictationTransport {
  let route: DictationTransport | null = null;
  // A cancel while the code loads must not leave a session behind it.
  let epoch = 0;
  const load = async () => {
    if (route) return route;
    const [{ createRelay }, { pcmCapture }] = await Promise.all([
      import("./relay.ts"),
      import("./audio.ts"),
    ]);
    return (route ??= createRelay({
      // Unknown is worth a try: the socket refuses what the viewer has not said.
      available: () => engine?.available !== false,
      socket: openDictationSocket,
      capture: pcmCapture,
    }));
  };
  return {
    picksDevice: false,
    available: () => engine?.available !== false && relaySupported(),
    async start(deviceId, events, stream) {
      const mine = ++epoch;
      const loaded = await load();
      if (mine === epoch) await loaded.start(deviceId, events, stream);
    },
    switchStream: (stream, deviceId) => route?.switchStream?.(stream, deviceId),
    flush: () => route?.flush() ?? Promise.resolve(),
    cancel() {
      epoch++;
      route?.cancel();
    },
  };
}

const BROWSER = webSpeechLocalTransport();
const ROUTES: readonly DictationTransport[] = [relayTransport(), BROWSER];

/** Why the mic is held, in the engine's reason's words. */
export const DICTATION_HELD: Record<DictationReason, string> = {
  disabled: "Dictation is turned off on this server",
  unsupported_platform: "This server cannot turn speech into text",
  model_unavailable: "This server could not load its speech model",
  failed: "Dictation could not start on this server",
};

/** Where an administrator reads how to turn dictation on or repair it. */
export const DICTATION_GUIDE = "https://docs.crewlet.ai/engine/latest/guides/dictation/";

const LABELS = { unavailable: "Dictation is not available right now" };

/**
 * Whether the browser has on-device recognition to stand in, from the API's
 * presence alone: asking it (`available()`) is the press's, as in the kit,
 * and a headless renderer has been seen to crash on the question.
 */
function browserRecognises(): boolean {
  if (typeof window === "undefined") return false;
  const w = window as unknown as Record<
    string,
    { available?: unknown; prototype?: object } | undefined
  >;
  const recognition = w.SpeechRecognition ?? w.webkitSpeechRecognition;
  return (
    typeof recognition?.available === "function" &&
    "processLocally" in (recognition.prototype ?? {})
  );
}

/**
 * The `dictation` every composer passes: one value per viewer answer. An
 * administrator is offered the guide beside the reason; a member the reason alone.
 */
export function useDictation(): DictationSource {
  const viewer = useViewer();
  const answer = viewer.dictation;
  useEffect(() => {
    engine = answer;
  }, [answer]);
  const down = answer?.available === false;
  const reason = down && !browserRecognises() ? (answer?.reason ?? "failed") : undefined;
  const admin = viewer.admin;
  return useMemo<DictationSource>(
    () => ({
      transport: ROUTES,
      labels: LABELS,
      unavailableReason: reason === undefined ? undefined : DICTATION_HELD[reason],
      setup:
        reason !== undefined && admin
          ? {
              label: "How to set up dictation",
              onSelect: () => window.open(DICTATION_GUIDE, "_blank", "noopener"),
            }
          : undefined,
    }),
    [reason, admin],
  );
}

/** Why an assistant's change to a box waits while the box takes dictation. */
export const DICTATING =
  "This box is taking dictation. Accept or cancel it, then apply the change.";

/**
 * The `dictation` for a box the assistant can also fill, and what its draft
 * box refuses with while a session runs there: a proposal waits for Accept or
 * Cancel, so dictated words never land beside a change made under them.
 */
export function useDictationBox(): { dictation: DictationSource; refusal: string | null } {
  const base = useDictation();
  const [live, setLive] = useState(false);
  const dictation = useMemo<DictationSource>(() => ({ ...base, onActiveChange: setLive }), [base]);
  return { dictation, refusal: live ? DICTATING : null };
}
