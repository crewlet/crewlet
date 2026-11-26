/**
 * The worklet's arithmetic: any microphone rate becomes 16 kHz PCM16 mono,
 * in whole frames, at the right level, with nothing lost between quanta.
 */
import { describe, expect, test } from "vitest";
import { PCM_FRAME_MS, PCM_RATE, Pcm16Framer } from "./pcm.ts";

const QUANTUM = 128;

/** `seconds` of a constant level, fed in render quanta at `rate`. */
function feed(
  framer: Pcm16Framer,
  rate: number,
  seconds: number,
  level: (i: number) => number,
  channels = 1,
) {
  const frames: ArrayBuffer[] = [];
  const total = Math.round(rate * seconds);
  for (let at = 0; at < total; at += QUANTUM) {
    const n = Math.min(QUANTUM, total - at);
    const quantum = Array.from({ length: channels }, () => new Float32Array(n));
    for (let i = 0; i < n; i++) for (const channel of quantum) channel[i] = level(at + i);
    frames.push(...framer.push(quantum));
  }
  return frames;
}

describe("a framer", () => {
  test.each([48_000, 44_100, 16_000])("turns %i Hz into 40 ms frames of 16 kHz samples", (rate) => {
    const frames = feed(new Pcm16Framer(rate), rate, 1, () => 0.5);
    const samples = (PCM_RATE * PCM_FRAME_MS) / 1000;
    expect(frames).toHaveLength(1000 / PCM_FRAME_MS);
    for (const frame of frames) expect(frame.byteLength).toBe(samples * 2);
    const first = new Int16Array(frames[0]!);
    expect(first[0]).toBe(Math.round(0.5 * 0x7fff));
    expect(first[samples - 1]).toBe(Math.round(0.5 * 0x7fff));
  });

  test("averages each output sample's interval, so a tone above 8 kHz cancels out", () => {
    // 12 kHz at 48 kHz is +1, 0, -1, 0: each three-sample interval sums near zero.
    const frames = feed(new Pcm16Framer(48_000), 48_000, 0.2, (i) => Math.sin((i * Math.PI) / 2));
    const peak = Math.max(...frames.flatMap((f) => [...new Int16Array(f)].map(Math.abs)));
    expect(peak).toBeLessThan(0x7fff / 2);
  });

  test("mixes the channels down to one", () => {
    const framer = new Pcm16Framer(16_000);
    const out: ArrayBuffer[] = [];
    for (let i = 0; i < 5; i++)
      out.push(...framer.push([new Float32Array(128).fill(1), new Float32Array(128).fill(0)]));
    expect(new Int16Array(out[0]!)[0]).toBe(Math.round(0.5 * 0x7fff));
  });

  test("clamps past full scale and keeps the negative end", () => {
    const loud = feed(new Pcm16Framer(16_000), 16_000, PCM_FRAME_MS / 1000, () => 3);
    const quiet = feed(new Pcm16Framer(16_000), 16_000, PCM_FRAME_MS / 1000, () => -3);
    expect(new Int16Array(loud[0]!)[0]).toBe(0x7fff);
    expect(new Int16Array(quiet[0]!)[0]).toBe(-0x8000);
  });

  test("refuses a microphone slower than the recognizer", () => {
    expect(() => new Pcm16Framer(8_000)).toThrow(RangeError);
  });
});
