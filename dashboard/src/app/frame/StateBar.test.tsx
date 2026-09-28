/**
 * The one strip that reports a degraded connection, and the order its causes
 * are ranked in.
 */

import { describe, expect, test } from "vitest";
import { degradationOf } from "./StateBar.tsx";

const noop = () => {};

describe("degradationOf", () => {
  test("an unverifiable session says the tab is paused and needs nothing", () => {
    // Distinct from a dropped connection: the socket is open and the node is
    // healthy, so "reconnecting" would be false, and the remedy is waiting —
    // the engine re-checks every minute on its own.
    const got = degradationOf({
      authRejected: false,
      connected: true,
      identityUnverifiable: true,
      configured: true,
      onSignIn: noop,
      onConfig: noop,
    });
    expect(got?.message).toMatch(/cannot verify your session/);
    expect(got?.action).toBeUndefined();
  });

  // THE REPAIR IS A SIGN-IN, the reader's own: there is no token to set any
  // more, and a strip naming one sent a person to look for a credential the
  // browser no longer holds anywhere.
  test("nobody signed in offers a sign-in", () => {
    let asked = false;
    const got = degradationOf({
      authRejected: true,
      connected: false,
      configured: true,
      onSignIn: () => {
        asked = true;
      },
      onConfig: noop,
    });
    expect(got?.action?.label).toBe("Sign in");
    got?.action?.onClick();
    expect(asked).toBe(true);
  });

  test("nobody signed in outranks an unverifiable session", () => {
    // A refusal never clears on its own; a hold does. The strip shows the one
    // somebody has to act on.
    const got = degradationOf({
      authRejected: true,
      connected: true,
      identityUnverifiable: true,
      configured: true,
      onSignIn: noop,
      onConfig: noop,
    });
    expect(got?.variant).toBe("danger");
  });

  test("refused access says why, outranks a dropped connection, and offers a retry", () => {
    // The socket stopped reconnecting — every dial would be refused by the
    // same decision — so "reconnecting" would be a lie, and the repair is an
    // administrator's rather than a sign-in the reader can perform.
    let retried = false;
    const got = degradationOf({
      authRejected: false,
      accessRefused: "grant withdrawn: state:read",
      connected: false,
      configured: true,
      onSignIn: noop,
      onRetry: () => {
        retried = true;
      },
      onConfig: noop,
    });
    expect(got?.variant).toBe("danger");
    expect(got?.message).toMatch(/grant withdrawn: state:read/);
    expect(got?.message).toMatch(/administrator/);
    got?.action?.onClick();
    expect(retried).toBe(true);
  });

  test("a verified, connected, configured tab has nothing to report", () => {
    expect(
      degradationOf({
        authRejected: false,
        connected: true,
        identityUnverifiable: false,
        configured: true,
        onSignIn: noop,
        onConfig: noop,
      }),
    ).toBeNull();
  });
});
