// @vitest-environment node

/**
 * Where each OAuth client a moved address left behind is signed in again, and
 * the client a page asks about read off the engine's redirect notice.
 */

import { expect, test } from "vitest";
import type { AdvisorNotice, AdvisorRegisteredClient } from "~/protocol/advisor.ts";
import { hostOf, signInPlace, staleClientOf } from "./redirectMoved.ts";

const google: AdvisorRegisteredClient = {
  vendor: "google",
  id: "123456789012-abcdefghij.apps.googleusercontent.com",
  label: "123456789012-abcdefgh…",
  uses: [
    { name: "adsense", title: "AdSense" },
    { name: "admob", title: "AdMob" },
  ],
  redirect: "https://old-tunnel.example.com/webhooks/mcp-oauth",
  at: "2026-01-01T09:00:00Z",
};
const server: AdvisorRegisteredClient = {
  vendor: "mcp",
  id: "search-client",
  label: "search",
  uses: [{ name: "search", title: "search" }],
  redirect: "https://old-tunnel.example.com/webhooks/mcp-oauth",
  at: "2026-01-01T09:00:00Z",
};
const notices: AdvisorNotice[] = [
  { class: "alarm", subject: { kind: "alarm", name: "backup_age" } },
  {
    class: "oauth_redirect_moved",
    subject: { kind: "company" },
    redirect: { uri: "https://crewlet.example.com/webhooks/mcp-oauth", clients: [google, server] },
  },
];

test("a Google client is signed in again on its first product's settings, a server on its Connection tab", () => {
  expect(signInPlace(google)).toEqual({
    path: ["company", "integrations", "adsense"],
    query: { open: "settings" },
    label: "Open AdSense",
  });
  expect(signInPlace(server)).toEqual({
    path: ["settings", "mcp-tools", "servers", "search"],
    query: { tab: "connection" },
    label: "Open search",
  });
});

test("a page finds its client by any product sharing it, and nothing while none is stale", () => {
  expect(staleClientOf(notices, "google", "admob")).toBe(google);
  expect(staleClientOf(notices, "mcp", "search")).toBe(server);
  expect(staleClientOf(notices, "google", "google_play")).toBeUndefined();
  expect(staleClientOf(notices, "google", "search")).toBeUndefined();
  expect(staleClientOf(notices.slice(0, 1), "google", "adsense")).toBeUndefined();
  expect(staleClientOf(undefined, "google", "adsense")).toBeUndefined();
});

test("an address is said by its host", () => {
  expect(hostOf("https://old-tunnel.example.com/webhooks/mcp-oauth")).toBe(
    "old-tunnel.example.com",
  );
  expect(hostOf("not an address")).toBe("not an address");
});
