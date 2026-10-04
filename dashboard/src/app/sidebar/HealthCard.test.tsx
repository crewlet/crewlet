/**
 * The engine's state at the foot of the sidebar: ONE state, chosen by
 * precedence, and never "healthy" on the strength of nothing.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { HealthCard, healthReading } from "./HealthCard.tsx";
import type { EngineHealth } from "~/contract/health.ts";

afterEach(cleanup);

const healthy: EngineHealth = {
  status: "healthy",
  configured: true,
  posture: "serve",
  nodes: 3,
  applied_epoch: 42,
  alarms: { count: 0 },
};

const read = (over: Partial<Parameters<typeof healthReading>[0]> = {}) =>
  healthReading({ connected: true, authRejected: false, health: healthy, ...over });

describe("which state the card says", () => {
  test("healthy names the fleet's size and the epoch it runs", () => {
    expect(read()).toEqual({
      tone: "success",
      title: "Engine healthy",
      detail: "3 nodes · config epoch 42",
    });
  });

  // THE ORDER IS PRECEDENCE: each state below is true AT THE SAME TIME as
  // every state after it, and the card says the one a reader acts on.
  test("nobody signed in outranks everything, and is the one state a press can fix", () => {
    const r = read({
      authRejected: true,
      accessRefused: "seat_unavailable",
      connected: false,
      health: { ...healthy, shutting_down: true },
    });
    expect(r.title).toBe("Not signed in");
    expect(r.press).toBe("sign-in");
  });

  // A PERSON THE ENGINE KNOWS AND REFUSES is not asked to sign in again —
  // signing in reaches the same person with the same access — so the card
  // says what happened and offers nothing a press could fix.
  test("a refusal of somebody the engine knows is said, and leads nowhere", () => {
    const r = read({ accessRefused: "seat_unavailable", connected: false });
    expect(r.title).toBe("Access refused");
    expect(r.detail).toContain("seat_unavailable");
    expect(r.press).toBe("none");
  });

  test("a lost socket outranks what the last push claimed", () => {
    expect(read({ connected: false, health: { ...healthy, shutting_down: true } }).title).toBe(
      "Reconnecting",
    );
  });

  test("draining outranks no configuration, which outranks a diverged posture", () => {
    const all = { ...healthy, shutting_down: true, configured: false, posture: "shed" };
    expect(read({ health: all }).title).toBe("Draining");
    expect(read({ health: { ...all, shutting_down: false } }).title).toBe("No configuration");
    expect(read({ health: { ...all, shutting_down: false, configured: true } }).title).toBe(
      "Engine shed",
    );
    expect(read({ health: { ...healthy, posture: "wait" } }).title).toBe("Engine healthy");
  });

  // NOTHING PUSHED IS NOT HEALTHY. Connected before the first health push, the
  // card had every field undefined and read that as "Engine healthy".
  test("no health push yet is waiting, not healthy", () => {
    const r = read({ health: null });
    expect(r.title).toBe("Waiting for the engine");
    expect(r.tone).not.toBe("success");
  });

  test("a failed presence read is named, never counted as zero nodes", () => {
    expect(read({ health: { ...healthy, nodes: undefined } }).detail).toBe(
      "node count unavailable · config epoch 42",
    );
  });

  // THE DOT AND THE TITLE ARE THE POSTURE. A standing alarm is a second fact
  // on its own line with its own tone: borrowing the dot for it drew "Engine
  // healthy" beside an amber dot, and retitling the posture to match made the
  // title a copy of the alarm line.
  //
  // AND "HEALTHY" IS A CLAIM THE ALARMS REFUTE: over a standing alarm the
  // title says the posture — serving — and never "healthy".
  test("a standing alarm keeps the posture's dot, and is its own warning line", () => {
    const r = read({ health: { ...healthy, alarms: { count: 2, worst: "trim_stalled" } } });
    expect(r.tone).toBe("success");
    expect(r.title).toBe("Engine serving");
    expect(r.detail).toBe("3 nodes · config epoch 42");
    expect(r.alarms).toEqual({ tone: "warning", text: "2 alarms · oldest: trim stalled" });
    expect(read({ health: { ...healthy, alarms: { count: 1 } } }).alarms?.text).toBe("1 alarm");
  });

  // TWO LOGS' `trim_blocked` ARE TWO CONDITIONS with two remedies, so a per-log
  // alarm names the log it stands on beside its kind.
  test("the oldest alarm names the log it stands on, where it stands on one", () => {
    const r = read({
      health: { ...healthy, alarms: { count: 1, worst: "trim_blocked", worst_domain: "tracker" } },
    });
    expect(r.alarms?.text).toBe("1 alarm · oldest: trim blocked (tracker)");
  });

  // AN ABSENT TABLE IS NOT AN EMPTY ONE: the engine has not evaluated it yet,
  // which is said — and is neither nothing firing nor a warning.
  test("alarms the engine has not evaluated are said, in a neutral line", () => {
    const r = read({ health: { ...healthy, alarms: undefined } });
    expect(r.alarms).toEqual({ tone: "neutral", text: "Alarms not evaluated yet" });
    expect(r.tone).toBe("success");
    expect(r.title).toBe("Engine serving");
  });

  test("a diverged posture still carries the alarm line beneath its own state", () => {
    const r = read({ health: { ...healthy, posture: "shed", alarms: { count: 1 } } });
    expect(r.tone).toBe("danger");
    expect(r.alarms?.tone).toBe("warning");
  });
});

describe("what the card is", () => {
  test("a link to the nodes, for every state that is a fact about the fleet", () => {
    render(<HealthCard reading={read()} onSignIn={() => {}} />);
    const card = screen.getByRole("link", { name: /Engine healthy/ });
    expect(card.getAttribute("href")).toBe("#/settings/nodes");
  });

  test("the alarm line is drawn with its own tone and a glyph, not by colour alone", () => {
    const { container } = render(
      <HealthCard
        reading={read({ health: { ...healthy, alarms: { count: 5 } } })}
        onSignIn={() => {}}
      />,
    );
    const card = container.querySelector(".health-card");
    expect(card?.getAttribute("data-tone")).toBe("success");
    // THE DOT SITS ON THE TITLE'S LINE, in its own box, rather than centred
    // on the card, where it drifted beside the alarm line.
    expect(card?.querySelector(".health-card-dot")?.firstElementChild).not.toBeNull();
    const line = container.querySelector(".health-card-alarm");
    expect(line?.textContent).toBe("5 alarms");
    expect(line?.getAttribute("data-tone")).toBe("warning");
    expect(line?.querySelector("svg")).not.toBeNull();
  });

  test("the button that goes to sign in, when nobody is signed in", () => {
    const onSignIn = vi.fn();
    render(<HealthCard reading={read({ authRejected: true })} onSignIn={onSignIn} />);
    expect(screen.queryByRole("link")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Not signed in/ }));
    expect(onSignIn).toHaveBeenCalledOnce();
  });

  test("a statement and nothing to press, when the engine refuses somebody it knows", () => {
    render(
      <HealthCard reading={read({ accessRefused: "seat_unavailable" })} onSignIn={() => {}} />,
    );
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("Access refused")).toBeTruthy();
  });
});
