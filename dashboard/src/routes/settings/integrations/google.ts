/**
 * The Google page's reading: the company's one Google connection (Google
 * Cloud's identity project), its reader and sign-in client, and each Google
 * product with its switch, drawn from the tools the integrations page already
 * reads. No second classifier: a product's state is its own tool's bucket,
 * and its next step is the finding or requirement that bucket came from.
 */

import { GOOGLE_PRODUCTS, type GoogleProduct } from "~/app/routes.ts";
import type { SetupRequirement, SetupToolState } from "~/contract/setup.ts";
import { connectionUnfinished, type Issue, type Model, type Tool } from "./model.ts";

/** Where the connection stands. */
export type ConnectionState = "not_set" | "unfinished" | "connected" | "failing";

/** Where one product stands, as its row says it. */
export type ProductState = "off" | "waits" | "turning_on" | "needs_you" | "on";

/** The one thing a product needs from a person next. */
export type NextStep =
  | { kind: "client" }
  | { kind: "sign_in"; req: SetupRequirement }
  | { kind: "reader"; req: SetupRequirement }
  | { kind: "issue"; issue: Issue };

export interface GoogleProductRow {
  kind: GoogleProduct;
  tool: Tool;
  /** Google Cloud comes with the connection: no switch of its own. */
  switchable: boolean;
  enabled: boolean;
  state: ProductState;
  next?: NextStep | undefined;
  /** The company reader's step at this product, for one that reads with it. */
  reader?: SetupRequirement | undefined;
  /** Its Google sign-in, for one reached through one or offering one. */
  signIn?: SetupRequirement | undefined;
}

export interface GooglePlace {
  cloud: Tool;
  state: ConnectionState;
  /** What is wrong with the connection itself, said above the products. */
  issues: Issue[];
  /** The connection's own fields: its form, without the sign-in client's. */
  connection: SetupToolState | undefined;
  /** The sign-in client's fields (the `sign_in` group), and whether it is set. */
  client: SetupRequirement[];
  clientSet: boolean;
  /** The identity project's ID, "" before one is named. */
  project: string;
  readerEmail: string;
  products: GoogleProductRow[];
}

/** The requirement group the sign-in client's fields are asked in. */
export const SIGN_IN_GROUP = "sign_in";

/** The subjects of the findings a product raises about the shared pieces. */
const CONNECTION_SUBJECT = "integrations.google_cloud.provisioning";
const CLIENT_SUBJECT = "integrations.google_cloud.sign_in_client";

/** Whether `issue` names a finding about `subject` (its key ends with it). */
const about = (issue: Issue, subject: string) => issue.key.endsWith(`:${subject}`);

/** The company section of a tool: its own block, never an agent's. */
function companySection(tool: Tool): SetupToolState | undefined {
  return tool.sections.find((s) => s.seat === undefined)?.tool;
}

/** The identity project an answer names: its own field, else the provisioner's address. */
export function identityProjectOf(section: SetupToolState | undefined): string {
  const reqs = section?.requirements ?? [];
  const own = reqs.find((r) => r.field === "identity_project")?.value;
  if (own) return own;
  const provisioner = reqs.find((r) => r.field === "provisioner")?.value ?? "";
  return /@([a-z][a-z0-9-]*)\.iam\.gserviceaccount\.com$/.exec(provisioner.trim())?.[1] ?? "";
}

/** The Google page's reading, or null where this build serves no Google Cloud. */
export function googlePlace(model: Pick<Model, "byKey">): GooglePlace | null {
  const cloud = model.byKey.get("google_cloud");
  if (!cloud) return null;
  const section = companySection(cloud);
  const reqs = section?.requirements ?? [];
  const client = reqs.filter((r) => r.group === SIGN_IN_GROUP);
  const issues = cloud.issues.filter((i) => !i.seat);
  const state: ConnectionState = !cloud.configured
    ? "not_set"
    : connectionUnfinished(cloud)
      ? "unfinished"
      : issues.some(
            (i) =>
              i.owner === "you" &&
              (i.kind === "credential_rejected" || i.kind === "credential_missing"),
          )
        ? "failing"
        : "connected";
  const products = GOOGLE_PRODUCTS.flatMap((kind) => {
    const tool = kind === "google_cloud" ? cloud : model.byKey.get(kind);
    return tool ? [productRow(kind, tool, state)] : [];
  });
  return {
    cloud,
    state,
    issues,
    connection: section && {
      ...section,
      requirements: reqs.filter((r) => r.group !== SIGN_IN_GROUP),
    },
    client,
    clientSet: client.some((r) => r.field === "sign_in_client_id" && r.present),
    project: identityProjectOf(section),
    readerEmail: products.map((p) => p.reader?.identity?.email).find((e): e is string => !!e) ?? "",
    products,
  };
}

/** One product's row: its switch, its state and its next step. */
function productRow(
  kind: GoogleProduct,
  tool: Tool,
  connection: ConnectionState,
): GoogleProductRow {
  const section = companySection(tool);
  const reqs = section?.requirements ?? [];
  const reader = reqs.find((r) => r.kind === "add_identity" && r.identity?.level === "company");
  const signIn = reqs.find((r) => r.kind === "consent");
  const switchable = kind !== "google_cloud";
  const enabled = switchable
    ? tool.configured && section?.enabled === true
    : connection !== "not_set";
  const row: GoogleProductRow = { kind, tool, switchable, enabled, state: "off", reader, signIn };
  if (!enabled) return row;
  if (switchable && connection !== "connected") return { ...row, state: "waits" };
  if (!switchable && (connection === "unfinished" || connection === "failing"))
    return { ...row, state: "waits" };
  const issues = tool.issues.filter((i) => !i.seat);
  const next = nextStep(issues, signIn, reader);
  switch (tool.bucket) {
    case "attention":
      return { ...row, state: "needs_you", next };
    case "working":
      return { ...row, state: "turning_on" };
    case "ready":
      // A READER NOBODY ADDED YET is the product's one step, even while its
      // own pass has nothing to say about it.
      return reader?.identity?.state === "missing"
        ? { ...row, state: "needs_you", next: { kind: "reader", req: reader } }
        : { ...row, state: "on" };
    default:
      return { ...row, state: "turning_on" };
  }
}

/** The first thing a product waiting on a person needs, in the order it is done. */
function nextStep(
  issues: Issue[],
  signIn: SetupRequirement | undefined,
  reader: SetupRequirement | undefined,
): NextStep | undefined {
  if (issues.some((i) => about(i, CLIENT_SUBJECT))) return { kind: "client" };
  if (issues.some((i) => about(i, CONNECTION_SUBJECT))) return undefined;
  // A CONSENT PRODUCT WAITING ON ITS SIGN-IN: its pass's credential finding,
  // which names no subject.
  const signInWait = issues.some(
    (i) =>
      i.owner === "you" &&
      (i.kind === "credential_missing" || i.kind === "credential_rejected") &&
      i.key.endsWith(`:${i.kind}:`),
  );
  if (signIn && signInWait) return { kind: "sign_in", req: signIn };
  if (reader?.identity?.state === "missing") return { kind: "reader", req: reader };
  const first = issues.find((i) => i.owner === "you") ?? issues[0];
  return first ? { kind: "issue", issue: first } : undefined;
}

/** Whether a product's issue is one its Google step already says, with the way to it. */
export function sharedGoogleIssue(issue: Issue, row: GoogleProductRow): boolean {
  if (about(issue, CONNECTION_SUBJECT) || about(issue, CLIENT_SUBJECT)) return true;
  return row.next?.kind === "sign_in" && issue.key.endsWith(`:${issue.kind ?? ""}:`);
}

/** Whether `kind` is reached only through a Google sign-in (AdMob, AdSense). */
export function needsSignIn(row: GoogleProductRow): boolean {
  return row.signIn?.required === true;
}
