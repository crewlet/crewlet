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

  // A SHAREABLE TAB SURVIVES, AND NOTHING ELSE: an integration's tab is a read
  // view held to its own names, so a pasted link lands on it; a form (`edit=`),
  // an unknown key or a tab the page does not have is dropped, as every query
  // on a page that lists no tab is. Mutation: keep every key, or every value,
  // and a case fails.
  test("keeps only a tab the page lists as shareable, and never a form", () => {
    const page = "#/company/integrations/google_cloud";
    expect(appRoute(`${page}?tab=settings`)?.canonical).toBe(`${page}?tab=settings`);
    expect(appRoute(`${page}?tab=settings&edit=settings`)?.canonical).toBe(`${page}?tab=settings`);
    expect(appRoute(`${page}?edit=key`)?.canonical).toBe(page);
    expect(appRoute(`${page}?tab=edit`)?.canonical).toBe(page);
    expect(appRoute(`${page}?tab=agents&provisioner=x%40evil.example`)?.canonical).toBe(
      `${page}?tab=agents`,
    );
    expect(appRoute("#/company/org/seats/pm?tab=settings")?.canonical).toBe(
      "#/company/org/seats/pm",
    );
  });
});
