import { describe, expect, test } from "vitest";

import { appRoute } from "./appRoute.ts";

describe("a page of this dashboard's own", () => {
  test("is a route the frame draws, opened rebuilt from its path alone", () => {
    const room = "0b9a3f2e-5c1d-4e7a-9b2c-1d2e3f4a5b6c";
    const target = appRoute(`#/chatlet/${room}?message=m-9`);
    expect(target?.path).toEqual(["chatlet", room]);
    expect(target?.query).toEqual({ message: "m-9" });
    // NO QUERY A WRITER CHOSE rides a written link.
    expect(target?.canonical).toBe(`#/chatlet/${room}`);
  });

  test("is the same page when written out whole on this origin, as a Copy link writes it", () => {
    const whole = `${location.origin}${location.pathname}#/inbox`;
    expect(appRoute(whole)?.canonical).toBe("#/inbox");
  });

  test("is never another origin, a query before the fragment, or a sign-in screen", () => {
    expect(appRoute("https://elsewhere.example/#/inbox")).toBeNull();
    expect(appRoute(`${location.origin}${location.pathname}?x=1#/inbox`)).toBeNull();
    expect(appRoute("#/signin")).toBeNull();
    expect(appRoute("#//evil.example")).toBeNull();
    expect(appRoute("#/inbox\\..")).toBeNull();
    expect(appRoute("javascript:alert(1)")).toBeNull();
  });
});
