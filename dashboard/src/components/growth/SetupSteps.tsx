/**
 * The two setup steps the growth products add to a connect form, each drawn
 * from what the engine says the step is and where it stands:
 *
 *  - `add_identity`: an identity Crewlet acts as (a seat's growth identity, or
 *    the company's read identity) that a person adds at the vendor, at the
 *    role named, in the place named. Only a person can, so the step shows the
 *    email to copy, the role and where, and whether the engine has seen it.
 *  - `consent`: a product that refuses service accounts (AdSense, AdMob) is
 *    reached through one company sign-in, which an admin gives at a fresh
 *    step-up by signing in with Google and allowing read access.
 */

import { Button, Copyable, Tag, Text, VendorUI } from "@crewlethq/ui";
import type { VendorTextPart } from "~/contract/vendortext.ts";
import { VendorWords } from "~/ui/VendorWords.tsx";
import "~/styles/growth.css";

/** Where an identity step stands: seen at the vendor, seen missing, or not looked at yet (`setup.IdentityStep.State`). */
export type IdentityState = "present" | "missing" | "unknown";

/** A `add_identity` requirement's step (`setup.IdentityStep`). */
export interface AddIdentityStep {
  /** The email the person adds at the vendor. */
  email: string;
  /** The vendor's role to give it: its own ID and its name. */
  role: { id: string; name: string };
  /** Where at the vendor the person adds it: how to reach the page, the page linked. */
  where: VendorTextPart[];
  /** Whose identity it is: the company's read identity, or one seat's. */
  level: "company" | "seat" | (string & {});
  /** The seat's name, for a seat's identity. */
  seat?: string;
  state: IdentityState | (string & {});
}

const IDENTITY_TAG: Readonly<
  Record<IdentityState, { words: string; variant: "success" | "warning" | "neutral" }>
> = {
  present: { words: "Added", variant: "success" },
  missing: { words: "Not added yet", variant: "warning" },
  unknown: { words: "Not checked yet", variant: "neutral" },
};

export function AddIdentity({
  step,
  product,
  why,
}: {
  step: AddIdentityStep;
  product: string;
  why?: VendorTextPart[] | undefined;
}) {
  const tag = IDENTITY_TAG[step.state as IdentityState] ?? IDENTITY_TAG.unknown;
  const whose =
    step.level === "company"
      ? `The company's read identity, which collects ${product}'s figures for every board.`
      : `${step.seat ?? "This agent"}'s own identity at ${product}.`;
  return (
    <section className="growth-step" aria-label={`Add ${step.email} to ${product}`}>
      <div className="growth-step__head">
        <Text as="span" variant="label">
          Add this identity at {product}
        </Text>
        <Tag size="sm" variant={tag.variant}>
          {tag.words}
        </Tag>
      </div>
      <Text as="p" tone="secondary">
        {whose}
      </Text>
      <Copyable
        value={step.email}
        variant="block"
        monospace
        truncate={false}
        ariaLabel={`Copy ${step.email}`}
      />
      <dl className="growth-step__facts">
        <dt>Role</dt>
        <dd>
          <VendorUI path={step.role.name} />
          {why && why.length > 0 ? (
            <>
              : <VendorWords text={why} />
            </>
          ) : null}
        </dd>
        <dt>Where</dt>
        <dd>
          <VendorWords text={step.where} />
        </dd>
      </dl>
    </section>
  );
}

/** Where a consent grant stands. */
export type ConsentState =
  | { kind: "missing" }
  | { kind: "signed_in"; by: string; at: string }
  /** The vendor refused it: revoked, expired, or an app still in Testing. `words` are the vendor's, redacted. */
  | { kind: "rejected"; words: string };

export function ConsentStep({
  product,
  scope,
  state,
  onSignIn,
  disabledReason,
}: {
  product: string;
  /** What the sign-in allows, in words: "read access to AdSense". */
  scope: string;
  state: ConsentState;
  /** Starts the sign-in: the engine signs the state and sends the browser to Google. */
  onSignIn: () => void;
  /** Why this reader cannot start it (not an admin, or the engine has no external address). */
  disabledReason?: string | undefined;
}) {
  const again = state.kind !== "missing";
  return (
    <section className="growth-step" aria-label={`Sign in to ${product}`}>
      <div className="growth-step__head">
        <Text as="span" variant="label">
          Sign in with Google
        </Text>
        <Tag
          size="sm"
          variant={
            state.kind === "signed_in"
              ? "success"
              : state.kind === "rejected"
                ? "danger"
                : "warning"
          }
        >
          {state.kind === "signed_in"
            ? "Signed in"
            : state.kind === "rejected"
              ? "Refused"
              : "Not signed in"}
        </Tag>
      </div>
      {state.kind === "missing" ? (
        <Text as="p">
          Waiting for a person to sign in with Google and allow {scope}. Agents read {product}{" "}
          through this one sign-in, never their own.
        </Text>
      ) : state.kind === "signed_in" ? (
        <Text as="p">
          Signed in by {state.by} on {state.at}. Signing in again replaces this sign-in and revokes
          nothing.
        </Text>
      ) : (
        <Text as="p">
          The Google sign-in for {product} was revoked or expired: {state.words} Sign in again to
          restore access.
        </Text>
      )}
      <div>
        <Button
          size="small"
          variant={again ? "secondary" : "primary"}
          disabledReason={disabledReason}
          tooltip
          onClick={onSignIn}
        >
          {again ? "Sign in again" : "Sign in with Google"}
        </Button>
      </div>
    </section>
  );
}
