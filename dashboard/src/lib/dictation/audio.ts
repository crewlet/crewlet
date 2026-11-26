/**
 * The microphone stream the kit opened, as PCM16 frames from the worklet.
 */
import workletUrl from "./pcm.worklet.ts?worker&url";
import { PCM_PROCESSOR } from "./pcm.ts";
import type { PcmCapture } from "./relay.ts";

/** Starts frames from `stream`; the source node is swapped on a device change. */
export async function pcmCapture(
  stream: MediaStream,
  onFrame: (pcm: ArrayBuffer) => void,
): Promise<PcmCapture> {
  const context = new AudioContext();
  try {
    await context.audioWorklet.addModule(workletUrl);
    // One silent output into the destination keeps the node rendered in every engine.
    const node = new AudioWorkletNode(context, PCM_PROCESSOR, {
      numberOfOutputs: 1,
      outputChannelCount: [1],
    });
    node.port.onmessage = (event: MessageEvent<ArrayBuffer>) => onFrame(event.data);
    node.connect(context.destination);
    let source = context.createMediaStreamSource(stream);
    source.connect(node);
    await context.resume();
    return {
      switchStream(next) {
        source.disconnect();
        source = context.createMediaStreamSource(next);
        source.connect(node);
      },
      stop() {
        node.port.onmessage = null;
        source.disconnect();
        node.disconnect();
        void context.close();
      },
    };
  } catch (error) {
    void context.close();
    throw error;
  }
}
