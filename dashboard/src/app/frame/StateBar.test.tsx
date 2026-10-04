/**
 * The one strip that reports a degraded connection, and the order its causes
 * are ranked in.
 */

import { describe, expect, test } from "vitest";
import { degradationOf } from "./StateBar.tsx";

const noop = () => {};

describe("degradationOf", () => {
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

  // THE PERSON REFUSED IS THE PERSON WHO NEEDS TO LEAVE. The socket stops on
  // a refusal, so nothing else on the page learns who they are, and the
  // engine keeps `/auth/` open to their session for exactly this.
  test("refused access offers a sign-out beside the retry", () => {
    let signedOut = false;
    const got = degradationOf({
      authRejected: false,
      accessRefused: "your seat is no longer in the chart",
      connected: false,
      configured: true,
      onSignIn: noop,
      onRetry: noop,
      onSignOut: () => {
        signedOut = true;
      },
      onConfig: noop,
    });
    expect(got?.secondary?.label).toBe("Sign out");
    got?.secondary?.onClick();
    expect(signedOut).toBe(true);
    expect(got?.action?.label).toBe("Try again");
  });

  test("a connected, configured tab has nothing to report", () => {
    expect(
      degradationOf({
        authRejected: false,
        connected: true,
        configured: true,
        onSignIn: noop,
        onConfig: noop,
      }),
    ).toBeNull();
  });
});
