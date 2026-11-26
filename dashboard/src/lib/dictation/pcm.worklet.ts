/**
 * The audio worklet that turns the microphone into the socket's PCM frames.
 * Loaded by `addModule` from its own same-origin file (`?worker&url`): the
 * policy refuses a `blob:` or `data:` module, and Trusted Types any `Worker`.
 */
import { PCM_PROCESSOR, Pcm16Framer } from "./pcm.ts";

// The worklet scope's globals, which the DOM library does not declare.
declare const sampleRate: number;
declare class AudioWorkletProcessor {
  readonly port: MessagePort;
}
declare function registerProcessor(name: string, processor: typeof AudioWorkletProcessor): void;

class Pcm16Processor extends AudioWorkletProcessor {
  private readonly framer = new Pcm16Framer(sampleRate);

  process(inputs: Float32Array[][]): boolean {
    for (const frame of this.framer.push(inputs[0] ?? [])) this.port.postMessage(frame, [frame]);
    return true;
  }
}

registerProcessor(PCM_PROCESSOR, Pcm16Processor);
