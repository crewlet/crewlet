/**
 * App Store analytics' turn-on: which connections a company names, read off
 * its setup form, and each app's outcome in words.
 */
import type { SetupAppStoreAnalyticsApp, SetupToolState } from "~/contract/setup.ts";

/** A connection's issuer ID field, whose config path names the connection. */
const ISSUER = /^integrations\.app_store_connect\.connections\.([a-z][a-z0-9-]*)\.issuer_id$/;

/** The connections the setup form names with an issuer ID present, in its order. */
export function analyticsConnections(states: readonly SetupToolState[]): string[] {
  const out: string[] = [];
  for (const state of states) {
    for (const r of state.requirements) {
      const id = ISSUER.exec(r.config_path)?.[1];
      if (id && r.present && !out.includes(id)) out.push(id);
    }
  }
  return out;
}

/** One app's outcome as the card says it. */
export function analyticsLine(a: SetupAppStoreAnalyticsApp): string {
  switch (a.state) {
    case "on":
      return `App ${a.app}: turned on.`;
    case "already_on":
      return `App ${a.app}: already on.`;
    case "failed":
      return `App ${a.app}: not turned on${a.detail ? `: ${a.detail}` : "."}`;
  }
}

/** A key slot's private key field, whose config path names its connection and slot. */
const KEY =
  /^integrations\.app_store_connect\.connections\.([a-z][a-z0-9-]*)\.keys\.([a-z_]+)\.private_key$/;

/** One connected key: its connection, its slot and the form's label for it. */
export interface ConnectedKey {
  connection: string;
  slot: string;
  label: string;
}

/** The keys the setup form shows stored, each once, in its order. */
export function connectedKeys(states: readonly SetupToolState[]): ConnectedKey[] {
  const out: ConnectedKey[] = [];
  for (const state of states) {
    for (const r of state.requirements) {
      const m = KEY.exec(r.config_path);
      if (!m || !r.present) continue;
      const [, connection, slot] = m as unknown as [string, string, string];
      if (!out.some((k) => k.connection === connection && k.slot === slot)) {
        out.push({ connection, slot, label: r.label.replace(/ \(\.p8\)$/, "") });
      }
    }
  }
  return out;
}
