/**
 * A QR code a camera can read: the value it was handed, exactly, drawn dark on
 * a light plate with its quiet zone, at error-correction level M.
 *
 * Every positive case here is a real decode (`~/test/qr.ts`), and every one
 * has a control that proves the decode can fail: a scanner that read anything
 * as anything would pass the positives on its own.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { encode } from "uqr";
import { scan } from "~/test/qr.ts";
import { QR_DARK, QR_LIGHT, QUIET_ZONE, QrCode, modulePath, qrModules } from "./QrCode.tsx";

const LABEL = "QR code for your authenticator app";

/** Two second-factor URIs as the engine composes them, one longer than the other. */
const SHORT =
  "otpauth://totp/crewlet.example.com:jane.doe?algorithm=SHA1&digits=6&issuer=crewlet.example.com&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP";
const LONG =
  "otpauth://totp/crewlet.internal.example.com:firstname.middlename.lastname?algorithm=SHA1&digits=6&issuer=crewlet.internal.example.com&period=30&secret=KRSXG5CTMVRXEZLUKRSXG5CTMVRXEZLU";

function drawn(value: string): Element {
  render(<QrCode value={value} label={LABEL} />);
  return screen.getByRole("img", { name: LABEL });
}

/** The dark modules the drawing's path holds, as a matrix of the symbol's size. */
function darkModules(svg: Element, size: number): boolean[][] {
  const m = Array.from({ length: size }, () => Array<boolean>(size).fill(false));
  const d = svg.querySelector("path")?.getAttribute("d") ?? "";
  for (const [, x, y, n] of d.matchAll(/M(\d+) (\d+)h(\d+)v1h-\d+z/g)) {
    for (let i = 0; i < Number(n); i++) m[Number(y)]![Number(x) + i] = true;
  }
  return m;
}

afterEach(cleanup);

describe("a QR code", () => {
  test.each([
    { name: "a short URI", value: SHORT },
    { name: "a longer one", value: LONG },
  ])("scans to exactly the value it was handed: $name", ({ value }) => {
    expect(scan(drawn(value))).toBe(value);
  });

  // THE CONTROL for the case above: two values draw two symbols, so a drawing
  // that ignored its value — or a scanner that answered whatever it was
  // expecting — could not pass both.
  test("two values are two different drawings", () => {
    const a = drawn(SHORT).querySelector("path")?.getAttribute("d");
    cleanup();
    const b = drawn(LONG).querySelector("path")?.getAttribute("d");
    expect(a).toBeTruthy();
    expect(a).not.toBe(b);
  });

  test("is dark on its own light plate, with four modules of quiet zone", () => {
    const svg = drawn(SHORT);
    const size = qrModules(SHORT)!.length;
    expect(svg.getAttribute("viewBox")).toBe(
      `${-QUIET_ZONE} ${-QUIET_ZONE} ${size + 2 * QUIET_ZONE} ${size + 2 * QUIET_ZONE}`,
    );
    expect(QUIET_ZONE).toBe(4);
    const plate = svg.querySelector("rect")!;
    expect(plate.getAttribute("fill")).toBe(QR_LIGHT);
    expect([plate.getAttribute("x"), plate.getAttribute("y")]).toEqual(["-4", "-4"]);
    expect(plate.getAttribute("width")).toBe(String(size + 8));
    expect(svg.querySelector("path")!.getAttribute("fill")).toBe(QR_DARK);
    // The inks themselves: black on white, the most contrast a screen shows.
    expect(QR_LIGHT.toLowerCase()).toBe("#ffffff");
    expect(QR_DARK.toLowerCase()).toBe("#000000");
  });

  // THE SCANNER CAN TELL. Each mutation below is a drawing a phone would not
  // read, and the decode that passes the cases above refuses every one — so
  // those passes are evidence about the drawing rather than about the decoder.
  describe("and the scan refuses a drawing a camera would refuse", () => {
    function mutated(change: (svg: Element) => void): Element {
      const svg = drawn(SHORT).cloneNode(true) as Element;
      change(svg);
      return svg;
    }

    test("inverted for a dark theme", () => {
      const svg = mutated((s) => {
        s.querySelector("rect")!.setAttribute("fill", QR_DARK);
        s.querySelector("path")!.setAttribute("fill", QR_LIGHT);
      });
      expect(scan(svg)).toBeNull();
    });

    test("with no plate, on the dark theme's page", () => {
      expect(scan(mutated((s) => s.querySelector("rect")!.remove()))).toBeNull();
    });

    test("with no quiet zone", () => {
      const size = qrModules(SHORT)!.length;
      const svg = mutated((s) => {
        s.setAttribute("viewBox", `0 0 ${size} ${size}`);
        const plate = s.querySelector("rect")!;
        plate.setAttribute("x", "0");
        plate.setAttribute("y", "0");
        plate.setAttribute("width", String(size));
        plate.setAttribute("height", String(size));
      });
      // Cropped to the symbol on a dark page, the finder patterns run into
      // the ground: there is no light margin to find them against.
      expect(scan(svg)).toBeNull();
    });
  });

  test("is drawn at error-correction level M, read off its own format information", () => {
    // THE STANDARD'S LAYOUT, not the encoder's: the first copy of the format
    // information runs along row 8, its two highest bits at columns 0 and 1,
    // masked with 0b10 — so M (00) is dark then light. Read off the drawing,
    // so a level changed in `qrModules` fails here whatever the encoder does.
    const svg = drawn(SHORT);
    const m = darkModules(svg, qrModules(SHORT)!.length);
    const level = ((Number(m[8]![0]) << 1) | Number(m[8]![1])) ^ 0b10;
    expect(["M", "L", "H", "Q"][level]).toBe("M");
  });

  // THE CONTROL for the level read: it is not constant. Drawn at L, the same
  // bits say L, so the case above would see a level that moved.
  test("and the level read can tell another level", () => {
    const m = encode(SHORT, { ecc: "L", border: 0 }).data;
    const level = ((Number(m[8]![0]) << 1) | Number(m[8]![1])) ^ 0b10;
    expect(["M", "L", "H", "Q"][level]).toBe("L");
  });

  test("draws one run rectangle per run of dark modules, and nothing for a light one", () => {
    expect(
      modulePath([
        [true, true, false, true],
        [false, false, false, false],
        [false, true, true, true],
      ]),
    ).toBe("M0 0h2v1h-2zM3 0h1v1h-1zM1 2h3v1h-3z");
  });

  test("draws nothing for a value no QR code can hold, and does not throw", () => {
    // 2,331 bytes is the most a version 40 symbol holds at level M.
    const huge = "a".repeat(2332);
    expect(qrModules(huge)).toBeNull();
    render(<QrCode value={huge} label={LABEL} />);
    expect(screen.queryByRole("img", { name: LABEL })).toBeNull();
    // The control: the longest value that fits is still drawn.
    expect(qrModules("a".repeat(2331))).not.toBeNull();
  });
});
