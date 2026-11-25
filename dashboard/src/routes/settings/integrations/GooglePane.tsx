/**
 * Google: the company's one Google connection, said once. Products holds a
 * switch per product with its state and its one next step; Settings holds the
 * connection, the company reader and the Google sign-in client. Each
 * product's agents, access and activity stay on its own page.
 *
 * The bare address follows the integration page's rule: the landing before
 * anything is set, the connection's form while it is unfinished, Products
 * once it works (`address.ts`).
 */

import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Copyable,
  DescriptionList,
  EmptyValue,
  HeadingLevelProvider,
  List,
  Section,
  StatusDot,
  StatusLine,
  Switch,
  Tabs,
} from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import type { GoogleProduct, GoogleTab } from "~/app/routes.ts";
import { ConfirmModal } from "~/components/Modal.tsx";
import { AddIdentity, type AddIdentityStep } from "~/components/growth/SetupSteps.tsx";
import { pathSegment, rest, RestError } from "~/protocol/rest.ts";
import { VendorWords } from "~/ui/VendorWords.tsx";
import { RedirectStep } from "../RedirectStep.tsx";
import { ConsentSignIn, SetupForm } from "../SetupForm.tsx";
import { WATCH_MS } from "../recheck.ts";
import {
  GOOGLE,
  googleFormOf,
  googleProductOf,
  googleQuery,
  googleTabOf,
  integrationAddress,
  type GoogleForm,
} from "./address.ts";
import { googlePlace, needsSignIn, type GooglePlace, type GoogleProductRow } from "./google.ts";
import type { Model } from "./model.ts";
import { IssueRow, SettingsSummary } from "./parts.tsx";

const PATH = ["company", "integrations", GOOGLE];

/** Each state's word and tone on a product's row. */
const ROW_WORDS = {
  off: { label: "Off", tone: undefined },
  waits: { label: "Waiting for the connection", tone: "neutral" },
  turning_on: { label: "Turning on", tone: "info" },
  needs_you: { label: "Needs you", tone: "warning" },
  on: { label: "On", tone: "success" },
} as const;

/** The connection's word in the header and the directory. */
export function googleWords(place: GooglePlace | null) {
  switch (place?.state) {
    case "connected":
      return { label: "Connected", tone: "success" as const };
    case "unfinished":
      return { label: "Setting up", tone: "info" as const };
    case "failing":
      return { label: "Needs attention", tone: "warning" as const };
    default:
      return null;
  }
}

/** A product's state on its line in the directory, nothing while it is off. */
export function googleLineWords(row: GoogleProductRow | undefined) {
  if (!row || row.state === "off") return null;
  const words = ROW_WORDS[row.state];
  return { label: row.state === "waits" ? "Waiting" : words.label, tone: words.tone ?? "neutral" };
}

/** Turns a product on or off, as its block's `enabled`; the page re-reads after. */
export async function switchProduct(model: Model, kind: string, on: boolean): Promise<string> {
  try {
    await rest.post(`/setup/integrations/${pathSegment(kind)}/inputs`, {
      values: { enabled: on ? "true" : "false" },
    });
    model.watch();
    return "";
  } catch (err) {
    return err instanceof RestError ? err.detail || err.message : String(err);
  }
}

export function GooglePane({ model, onDisconnect }: { model: Model; onDisconnect: () => void }) {
  const route = useRoute();
  const nav = useNavigator();
  const place = useMemo(() => googlePlace(model), [model]);
  const askedTab = googleTabOf(route.query.get("tab"));
  const askedForm = googleFormOf(route.query.get("edit"));
  const focus = googleProductOf(route.query.get("product"));
  const state = place?.state;
  const held = model.managed ?? place?.cloud.blocked;
  // THE BARE ADDRESS OF AN UNFINISHED CONNECTION IS ITS FORM, written in so a
  // reading that moves under it keeps it open.
  const defaultForm = !askedTab && !askedForm && state === "unfinished" && !held;
  useEffect(() => {
    if (defaultForm) nav.replace(PATH, googleQuery({ edit: "settings", product: focus }));
  }, [defaultForm, nav, focus]);
  const [checking, setChecking] = useState(false);
  const [refused, setRefused] = useState("");
  const landed = useRef<(() => void) | null>(null);
  useEffect(() => {
    if (state === "connected") landed.current?.();
  }, [state]);
  if (!place) return <p className="t-caption">This build serves no Google Cloud.</p>;

  const open: GoogleForm | null = held ? null : (askedForm ?? (defaultForm ? "settings" : null));
  const tab: GoogleTab = open ? "settings" : (askedTab ?? "products");
  const openForm = (form: GoogleForm) =>
    nav.replace(PATH, googleQuery({ edit: form, product: focus }));
  // CANCEL lands on the Settings tab, or the landing before a connect; a save
  // lands where the connection now stands, with the product it was opened for.
  const closeForm = (saved: boolean) =>
    nav.replace(
      PATH,
      saved || state === "not_set"
        ? googleQuery({ product: focus })
        : googleQuery({ tab: "settings" }),
    );
  // A CONNECT HAS LANDED WHEN THIS PAGE READS IT CONNECTED, or after the
  // recheck window, so the page never redraws from a reading older than the write.
  const afterWrite = (): Promise<void> => {
    model.watch();
    return new Promise((resolve) => {
      const timer = setTimeout(done, WATCH_MS);
      function done() {
        clearTimeout(timer);
        landed.current = null;
        resolve();
      }
      landed.current = done;
    });
  };

  const on = place.products.filter((p) => p.enabled).length;
  const waiting = place.products.some((p) => p.state === "needs_you");
  const words = googleWords(place);
  const facts: Fact[] | undefined =
    state === "not_set"
      ? undefined
      : [
          ...(place.project
            ? [{ label: "Identity project", value: place.project, token: true }]
            : []),
          { label: "Products on", value: `${on} of ${place.products.length}` },
        ];
  const header = (
    <ObjectHeader
      kind="Integration"
      icon="plug"
      mark={<VendorMark vendor="google" size={36} />}
      title="Google"
      status={words ? <StatusDot tone={words.tone}>{words.label}</StatusDot> : undefined}
      description="One connection for every Google product your agents use."
      facts={facts}
      actions={
        !open && (state === "not_set" || state === "unfinished") ? (
          <Button
            size="small"
            variant="primary"
            disabledReason={held}
            onClick={() => !held && openForm("settings")}
          >
            {state === "not_set" ? "Connect Google" : "Finish setup"}
          </Button>
        ) : undefined
      }
    />
  );

  const connectionForm = place.connection && (
    <SetupForm
      sections={[{ name: "Google", tool: place.connection }]}
      title="Google"
      onClose={() => closeForm(false)}
      onDone={afterWrite}
    />
  );

  if (state === "not_set") {
    return (
      <>
        {header}
        {open === "settings" ? connectionForm : <Landing place={place} focus={focus} />}
      </>
    );
  }

  const focusedRow = place.products.find((p) => p.kind === focus);
  return (
    <>
      {header}
      <div className="tab-detail">
        <Tabs
          variant="underline"
          ariaLabel="Google sections"
          value={tab}
          onValueChange={(next) => nav.to(PATH, googleQuery({ tab: next as GoogleTab }))}
          items={[
            {
              value: "products",
              label: "Products",
              count: on,
              ...(waiting || state === "failing"
                ? { marker: { tone: "warning" as const, label: "Needs attention" } }
                : {}),
            },
            {
              value: "settings",
              label: "Settings",
              ...(state === "failing" || state === "unfinished"
                ? { marker: { tone: "warning" as const, label: "Needs attention" } }
                : {}),
            },
          ]}
          actions={
            model.guarded ? undefined : (
              <Button
                size="small"
                variant="ghost"
                leadingIcon={<RotateCwGlyph size="sm" />}
                loading={checking}
                disabledReason={model.managed ?? undefined}
                onClick={async () => {
                  if (model.managed) return;
                  setChecking(true);
                  try {
                    const tools = place.products.filter((p) => p.enabled).map((p) => p.tool);
                    for (const tool of tools) {
                      const said = await model.check(tool);
                      if (said) {
                        setRefused(said);
                        break;
                      }
                    }
                  } finally {
                    setChecking(false);
                  }
                }}
              >
                Check now
              </Button>
            )
          }
        />
        {refused && (
          <p className="t-caption" role="status">
            {refused}
          </p>
        )}
        <HeadingLevelProvider level={3}>
          {tab === "products" && (
            <div className="int-tab">
              <SharedIssues place={place} onFinish={() => openForm("settings")} held={held} />
              <Section title="Products" description="Turn on what your agents use.">
                <ul className="int-google-products">
                  {place.products.map((row) => (
                    <ProductRow
                      key={row.kind}
                      row={row}
                      place={place}
                      model={model}
                      focused={focusedRow?.kind === row.kind}
                      onClient={() => openForm("client")}
                    />
                  ))}
                </ul>
              </Section>
            </div>
          )}
          {tab === "settings" && (
            <div className="int-tab">
              <Section
                title="Connection"
                description="Where Crewlet keeps every agent's Google identity."
                actions={
                  open !== "settings" && (
                    <Button
                      size="small"
                      variant="secondary"
                      disabledReason={held}
                      onClick={() => !held && openForm("settings")}
                    >
                      Edit connection
                    </Button>
                  )
                }
              >
                {open === "settings" ? (
                  connectionForm
                ) : (
                  <SettingsSummary
                    tool={{
                      ...place.cloud,
                      sections: place.connection
                        ? [{ name: "Google", tool: place.connection }]
                        : [],
                    }}
                  />
                )}
              </Section>
              <ReaderSection place={place} />
              <SignInSection
                place={place}
                editing={open === "client"}
                held={held}
                onEdit={() => openForm("client")}
                onClose={closeForm}
                onDone={afterWrite}
              />
              <div className="int-google-disconnect">
                <Button
                  size="small"
                  variant="danger"
                  disabledReason={model.managed ?? undefined}
                  onClick={() => !model.managed && onDisconnect()}
                >
                  Disconnect Google
                </Button>
                <p className="t-caption">
                  Every Google product stops until Google is connected again.
                </p>
              </div>
            </div>
          )}
        </HeadingLevelProvider>
      </div>
    </>
  );
}

/** What is wrong with the connection itself, said once above the products it holds up. */
function SharedIssues({
  place,
  onFinish,
  held,
}: {
  place: GooglePlace;
  onFinish: () => void;
  held: string | undefined;
}) {
  if (place.state === "unfinished") {
    return (
      <Callout
        variant="warning"
        title="Finish connecting Google"
        action={
          <Button
            size="small"
            variant="primary"
            disabledReason={held}
            onClick={() => !held && onFinish()}
          >
            Finish setup
          </Button>
        }
      >
        Products you turn on now start once Google is connected.
      </Callout>
    );
  }
  if (place.issues.length === 0) return null;
  return (
    <div className="int-issues">
      {place.issues.map((issue) => (
        <IssueRow
          key={issue.key}
          tool={place.cloud}
          // THE KEY IS CREWLET'S OWN: a refused connection is mended in its form.
          issue={
            issue.fix?.intent === "rotate"
              ? { ...issue, fix: { label: "Open the connection", intent: "settings" } }
              : issue
          }
          onFix={() => !held && onFinish()}
        />
      ))}
    </div>
  );
}

function ProductRow({
  row,
  place,
  model,
  focused,
  onClient,
}: {
  row: GoogleProductRow;
  place: GooglePlace;
  model: Model;
  focused: boolean;
  onClient: () => void;
}) {
  const ref = useRef<HTMLLIElement>(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [refused, setRefused] = useState("");
  useEffect(() => {
    if (focused) ref.current?.scrollIntoView({ block: "center" });
  }, [focused]);
  const words = ROW_WORDS[row.state];
  const name = row.tool.entry.name;
  // A SIGN-IN PRODUCT HAS NOTHING TO TURN ON without the client it signs in as.
  const noClient = needsSignIn(row) && !row.enabled && !place.clientSet;
  const held =
    model.managed ??
    row.tool.blocked ??
    (noClient ? "Add the Google sign-in client first, on the Settings tab." : undefined);
  const flip = async (next: boolean) => {
    if (!next) {
      setConfirm(true);
      return;
    }
    setBusy(true);
    setRefused(await switchProduct(model, row.kind, true));
    setBusy(false);
  };
  return (
    <li ref={ref} className="int-google-product" data-focused={focused || undefined}>
      <VendorMark
        vendor={row.tool.entry.vendor}
        size={24}
        aria-hidden
        className="int-google-mark"
      />
      <div className="int-google-main">
        {row.enabled ? (
          <a className="int-google-name" href={href(["company", "integrations", row.kind])}>
            {name}
          </a>
        ) : (
          <span className="int-google-name">{name}</span>
        )}
        <p className="t-caption int-google-does">{row.tool.entry.description}</p>
        <StatusLine
          label={words.label}
          tone={words.tone}
          pulse={row.state === "turning_on"}
          notes={
            row.enabled && row.state !== "waits"
              ? [
                  <a
                    key="agents"
                    className="t-link"
                    href={integrationAddress(row.kind, { tab: "agents" })}
                  >
                    Agents
                  </a>,
                ]
              : undefined
          }
        />
        <NextStep row={row} place={place} onClient={onClient} />
        {refused && (
          <p className="t-caption" role="alert">
            {refused}
          </p>
        )}
      </div>
      <div className="int-google-end">
        {row.switchable ? (
          <Switch
            label={name}
            labelHidden
            checked={row.enabled}
            disabled={busy}
            disabledReason={held}
            onCheckedChange={(next) => !held && void flip(next)}
          />
        ) : (
          <span className="t-caption">With the connection</span>
        )}
      </div>
      <ConfirmModal
        open={confirm}
        onClose={() => setConfirm(false)}
        onConfirm={async () => {
          setConfirm(false);
          setBusy(true);
          setRefused(await switchProduct(model, row.kind, false));
          setBusy(false);
        }}
        title={`Turn off ${name}?`}
        message={`Agents stop reaching ${name} until it is turned on again. Their levels and choices are kept.`}
        confirmLabel="Turn off"
        cancelLabel="Keep it on"
        destructive
      />
    </li>
  );
}

/** What one product needs from a person next, with the control that does it. */
function NextStep({
  row,
  place,
  onClient,
}: {
  row: GoogleProductRow;
  place: GooglePlace;
  onClient: () => void;
}) {
  const name = row.tool.entry.name;
  // GOOGLE ADS' API ACCESS IS THE IDENTITY PROJECT'S, applied for once.
  const adsAccess = row.kind === "google_ads" && row.enabled && place.project && (
    <p className="t-caption">
      Google grants Google Ads API access to the identity project: apply once for Explorer access on{" "}
      <a
        className="t-link"
        href={`https://console.cloud.google.com/apis/api/googleads.googleapis.com/overview?project=${encodeURIComponent(place.project)}`}
        target="_blank"
        rel="noreferrer"
      >
        its Google Ads API page
      </a>
      . Until Google grants it, agents reach only test accounts.
    </p>
  );
  const next = row.next;
  let step: ReactNode = null;
  if (row.state === "needs_you" && next) {
    switch (next.kind) {
      case "client":
        step = (
          <>
            <p className="t-body">
              {name} is reached through a Google sign-in, made as your own OAuth client. Add it
              once; every Google sign-in uses it.
            </p>
            <div className="int-google-actions">
              <Button size="small" variant="secondary" onClick={onClient}>
                Add the sign-in client
              </Button>
            </div>
          </>
        );
        break;
      case "sign_in":
        step = (
          <ConsentSignIn
            kind={row.kind}
            label="Sign in with Google"
            optional={false}
            signedIn={false}
            help={next.req.hint ? <VendorWords text={next.req.hint} /> : undefined}
          />
        );
        break;
      case "reader":
        step = next.req.identity && (
          <AddIdentity
            step={next.req.identity as AddIdentityStep}
            product={name}
            why={next.req.hint}
          />
        );
        break;
      case "issue":
        step = (
          <>
            <p className="t-body">{next.issue.problem}</p>
            {next.issue.remedy && <p className="t-caption">{next.issue.remedy}</p>}
            {next.issue.external && (
              <div className="int-google-actions">
                <ButtonLink
                  size="small"
                  variant="secondary"
                  external
                  href={next.issue.external.href}
                >
                  {next.issue.external.label}
                </ButtonLink>
              </div>
            )}
          </>
        );
        break;
    }
  } else if (row.state === "off" && needsSignIn(row) && !place.clientSet) {
    step = (
      <p className="t-caption">
        Reached through a Google sign-in:{" "}
        <button type="button" className="t-link int-google-inline" onClick={onClient}>
          add the sign-in client
        </button>{" "}
        first.
      </p>
    );
  }
  if (!step && !adsAccess) return null;
  return (
    <>
      {step && <div className="int-google-next">{step}</div>}
      {adsAccess}
    </>
  );
}

/** The page before anything is set: what one connection gives, and the products it holds. */
function Landing({ place, focus }: { place: GooglePlace; focus: GoogleProduct | null }) {
  const product = place.products.find((p) => p.kind === focus);
  return (
    <>
      {product && (
        <Callout
          variant="neutral"
          title={`${product.tool.entry.name} runs on your Google connection`}
        >
          Connect Google once. {product.tool.entry.name} is then a switch here, as is every other
          Google product.
        </Callout>
      )}
      <div className="int-before">
        <section aria-label="How it works">
          <h3 className="t-label">How it works</h3>
          <List variant="numbered">
            <List.Item>Choose a new, empty Google Cloud project kept for Crewlet.</List.Item>
            <List.Item>Run the block Crewlet shows in Cloud Shell, once.</List.Item>
            <List.Item>Paste the last line it prints, and save.</List.Item>
            <List.Item>
              Turn on the products your agents use. Crewlet turns on their APIs and makes the
              company reader.
            </List.Item>
          </List>
        </section>
        <section aria-label="Products on this connection">
          <h3 className="t-label">Products on this connection</h3>
          <ul className="int-google-products">
            {place.products.map((p) => (
              <li key={p.kind} className="int-google-product int-google-product--preview">
                <VendorMark
                  vendor={p.tool.entry.vendor}
                  size={24}
                  aria-hidden
                  className="int-google-mark"
                />
                <div className="int-google-main">
                  <span className="int-google-name">{p.tool.entry.name}</span>
                  <p className="t-caption int-google-does">{p.tool.entry.description}</p>
                </div>
              </li>
            ))}
          </ul>
        </section>
      </div>
    </>
  );
}

/** The company reader: made by Crewlet, added once at each product that reads with it. */
function ReaderSection({ place }: { place: GooglePlace }) {
  const readers =
    place.state === "connected"
      ? place.products.filter((p) => p.enabled && p.reader?.identity)
      : [];
  return (
    <Section
      title="Company reader"
      description="Reads figures for the whole company; changes nothing."
    >
      {readers.length === 0 ? (
        <p className="t-caption">
          Crewlet makes it once Google is connected and Google Analytics, Search Console, Google Ads
          or Google Play is on.
        </p>
      ) : (
        <>
          <p className="t-caption int-google-lead">
            Crewlet makes it. Add it once in each product, at the role below.
          </p>
          <Copyable
            value={place.readerEmail}
            variant="block"
            monospace
            ariaLabel="Copy the company reader's email"
          />
          {/* THE EMAIL ONCE, then each product's role and where it is added. */}
          <DescriptionList
            dense
            termWidth="minmax(8rem, 14rem)"
            items={readers.map((p) => [p.tool.entry.name, <ReaderRow row={p} />])}
          />
        </>
      )}
    </Section>
  );
}

const READER_STATE: Record<string, { label: string; tone?: "success" | "warning" }> = {
  present: { label: "Added", tone: "success" },
  missing: { label: "Not added yet", tone: "warning" },
  unknown: { label: "Not checked yet" },
};

/** One product's reader: its role, where it is added, and whether Crewlet saw it. */
function ReaderRow({ row }: { row: GoogleProductRow }) {
  const step = row.reader!.identity!;
  const state = READER_STATE[step.state] ?? READER_STATE.unknown!;
  return (
    <div className="int-google-reader">
      <StatusLine label={state.label} tone={state.tone} notes={[`at ${step.role.name}`]} />
      <span className="t-caption">
        <VendorWords text={step.where} />
      </span>
    </div>
  );
}

/** The one OAuth client every Google sign-in is made as, and each product's sign-in. */
function SignInSection({
  place,
  editing,
  held,
  onEdit,
  onClose,
  onDone,
}: {
  place: GooglePlace;
  editing: boolean;
  held: string | undefined;
  onEdit: () => void;
  onClose: (saved: boolean) => void;
  onDone: () => Promise<void>;
}) {
  const redirect = place.client.find((r) => r.kind === "redirect");
  const signIns = place.products.filter((p) => p.signIn);
  const section = place.connection && {
    ...place.connection,
    requirements: place.client,
    prepare: undefined,
    summary: undefined,
    caution: undefined,
  };
  return (
    <Section
      title="Google sign-in"
      description="One OAuth client for every Google sign-in."
      actions={
        !editing && (
          <Button
            size="small"
            variant="secondary"
            disabledReason={held}
            onClick={() => !held && onEdit()}
          >
            {place.clientSet ? "Edit client" : "Add client"}
          </Button>
        )
      }
    >
      <p className="t-caption int-google-lead">
        AdMob and AdSense are reached through a sign-in. Google Analytics and Google Play use one to
        add agents for you.
      </p>
      {editing && section ? (
        <SetupForm
          sections={[{ name: "Google sign-in", tool: section }]}
          title="Sign-in client"
          onClose={() => onClose(false)}
          onDone={async () => {
            await onDone();
            onClose(true);
          }}
        />
      ) : (
        <>
          {place.clientSet ? (
            <SettingsSummary
              tool={{
                ...place.cloud,
                sections: section ? [{ name: "Google sign-in", tool: section }] : [],
              }}
            />
          ) : (
            <EmptyValue label="No sign-in client yet" />
          )}
          {redirect && <RedirectStep r={redirect} />}
          {signIns.length > 0 && (
            <DescriptionList
              dense
              termWidth="minmax(8rem, 14rem)"
              items={signIns.map((p) => [
                p.tool.entry.name,
                <SignInState row={p} clientSet={place.clientSet} />,
              ])}
            />
          )}
        </>
      )}
    </Section>
  );
}

/** One product's sign-in: whether it is needed, and the control that makes it. */
function SignInState({ row, clientSet }: { row: GoogleProductRow; clientSet: boolean }) {
  const needed = needsSignIn(row);
  if (!row.enabled)
    return <StatusLine label="Off" notes={[needed ? "needed to reach it" : "optional"]} />;
  if (!clientSet) return <StatusLine label="Waits for the sign-in client" tone="neutral" />;
  // SIGNED IN is said only where the product's own pass reads clean through it.
  if (needed && row.state === "on") return <StatusLine label="Signed in" tone="success" />;
  return (
    <ConsentSignIn
      kind={row.kind}
      label={needed ? "Sign in with Google" : "Admin sign-in"}
      optional={!needed}
      signedIn={false}
      help={row.signIn?.hint ? <VendorWords text={row.signIn.hint} /> : undefined}
    />
  );
}
