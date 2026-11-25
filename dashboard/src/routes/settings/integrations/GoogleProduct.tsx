/**
 * What a Google product's own page draws of the Google page: its way on (Turn
 * on, in place once Google is connected), the shared step it waits on, and
 * the row back to Google. Its agents, access and activity stay its own.
 */

import { useState } from "react";
import { Button, ButtonLink, Callout, RelatedRow } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { googleAddress } from "./address.ts";
import type { GooglePlace, GoogleProductRow } from "./google.ts";
import { switchProduct } from "./GooglePane.tsx";
import type { Model } from "./model.ts";
import { actionName } from "./parts.tsx";

/** The page's way on: Turn on (here once Google is connected), or Google's own form. */
export function GoogleAction({
  row,
  place,
  model,
}: {
  row: GoogleProductRow;
  place: GooglePlace;
  model: Model;
}) {
  const [busy, setBusy] = useState(false);
  const [refused, setRefused] = useState("");
  const name = row.tool.entry.name;
  if (!row.switchable) {
    if (place.state !== "not_set" && place.state !== "unfinished") return null;
    return (
      <ButtonLink
        size="small"
        variant="primary"
        href={googleAddress({ edit: "settings", product: row.kind })}
      >
        {place.state === "not_set" ? "Connect Google" : "Finish setup"}
      </ButtonLink>
    );
  }
  if (row.enabled) return null;
  // TURNED ON HERE once the connection works; before, Google's page connects
  // it first, with this product's row waiting there.
  if (place.state !== "connected") {
    return (
      <ButtonLink size="small" variant="primary" href={googleAddress({ product: row.kind })}>
        Turn on in Google
      </ButtonLink>
    );
  }
  const held =
    model.managed ??
    row.tool.blocked ??
    (row.signIn?.required && !place.clientSet
      ? "Add the Google sign-in client first, on the Google page."
      : undefined);
  return (
    <>
      <Button
        size="small"
        variant="primary"
        aria-label={actionName("Turn on", name)}
        loading={busy}
        disabledReason={held}
        onClick={async () => {
          if (held) return;
          setBusy(true);
          setRefused(await switchProduct(model, row.kind, true));
          setBusy(false);
        }}
      >
        Turn on
      </Button>
      {refused && (
        <span className="t-caption" role="alert">
          {refused}
        </span>
      )}
    </>
  );
}

/** The shared step a product waits on, said once on its page with the way to it. */
export function GoogleStepCallout({ row, place }: { row: GoogleProductRow; place: GooglePlace }) {
  const name = row.tool.entry.name;
  const there = (label: string, at: Parameters<typeof googleAddress>[0]) => (
    <ButtonLink size="small" variant="secondary" href={googleAddress(at)}>
      {label}
    </ButtonLink>
  );
  if (row.state === "waits") {
    return (
      <Callout
        variant="warning"
        title={
          place.state === "failing"
            ? "Google's connection is failing"
            : "Waiting for Google's connection"
        }
        action={there("Open Google", { product: row.kind })}
      >
        {name} works once your company's Google connection does.
      </Callout>
    );
  }
  if (row.state === "turning_on") {
    return (
      <Callout variant="info" title={`Turning on ${name}`}>
        Crewlet is setting it up in your identity project. Its next step shows here.
      </Callout>
    );
  }
  if (row.state !== "needs_you" || !row.next || row.next.kind === "issue") return null;
  switch (row.next.kind) {
    case "client":
      return (
        <Callout
          variant="warning"
          title="Add the Google sign-in client"
          action={there("Add it in Google", { edit: "client", product: row.kind })}
        >
          {name} is reached through a Google sign-in, set up once for every Google product.
        </Callout>
      );
    case "sign_in":
      return (
        <Callout
          variant="warning"
          title="Sign in with Google"
          action={there("Open Google", { product: row.kind })}
        >
          {name} is reached through a person's Google sign-in, made on the Google page.
        </Callout>
      );
    case "reader":
      return (
        <Callout
          variant="warning"
          title="Add the company reader"
          action={there("Show me how", { product: row.kind })}
        >
          {name} does not list the company reader yet; it is added once, at its read role.
        </Callout>
      );
  }
}

/** The row back to Google, under a product's own settings. */
export function GoogleRelated({ row }: { row: GoogleProductRow }) {
  return (
    <RelatedRow
      href={googleAddress({ product: row.kind })}
      icon={<VendorMark vendor="google" size={16} aria-hidden />}
      title="Google"
      description="The connection, the company reader and the Google sign-in, set once for every Google product."
    />
  );
}
