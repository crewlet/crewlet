/**
 * Microphone audio as the dictation socket takes it: 16-bit mono PCM at
 * 16 kHz, in frames of `frameMs`. Pure, so the worklet and its tests share it.
 */

/** The rate the engine's recognizer takes (`speech.Rate`). */
export const PCM_RATE = 16_000;

/** The worklet's processor name, which `audio.ts` creates its node by. */
export const PCM_PROCESSOR = "crewlet-pcm16";

/** One frame's length: inside the 20 to 100 ms the socket takes. */
export const PCM_FRAME_MS = 40;

/**
 * Downmixes and decimates float samples at `inputRate` to PCM16 at
 * [PCM_RATE]. Each output sample is the mean of the input samples in its
 * interval, a box filter that keeps speech and drops what would alias.
 */
export class Pcm16Framer {
  private readonly step: number;
  private readonly frame: Int16Array;
  private filled = 0;
  private sum = 0;
  private count = 0;
  /** Input samples seen, and where the current output sample's interval ends. */
  private seen = 0;
  private edge: number;

  constructor(inputRate: number, frameMs: number = PCM_FRAME_MS) {
    if (!(inputRate >= PCM_RATE))
      throw new RangeError(`input rate ${inputRate} is under ${PCM_RATE}`);
    this.step = inputRate / PCM_RATE;
    this.edge = this.step;
    this.frame = new Int16Array(Math.round((PCM_RATE * frameMs) / 1000));
  }

  /** Takes one render quantum's channels; answers the frames it completed. */
  push(channels: readonly Float32Array[]): ArrayBuffer[] {
    const out: ArrayBuffer[] = [];
    const first = channels[0];
    if (!first) return out;
    for (let i = 0; i < first.length; i++) {
      let mixed = 0;
      for (const channel of channels) mixed += channel[i] ?? 0;
      this.sum += mixed / channels.length;
      this.count++;
      this.seen++;
      if (this.seen >= this.edge) {
        this.emit(this.sum / this.count, out);
        this.sum = 0;
        this.count = 0;
        this.edge += this.step;
      }
    }
    return out;
  }

  private emit(sample: number, out: ArrayBuffer[]): void {
    const clamped = Math.max(-1, Math.min(1, sample));
    this.frame[this.filled++] =
      clamped < 0 ? Math.round(clamped * 0x8000) : Math.round(clamped * 0x7fff);
    if (this.filled === this.frame.length) {
      out.push(this.frame.slice().buffer);
      this.filled = 0;
    }
  }
}
