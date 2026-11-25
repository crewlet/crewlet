/**
 * An agent's identity at the vendor, as the engine words it: the kit's
 * AgentIdentity heading the agent's box, and the kit's AgentIdentitySheet
 * that sets one up, the same for every vendor. Nothing here decides a state
 * or a word: the answer's `identity` carries the chip, the line, the problem,
 * the actions and the sheet's steps, so the tab and Ask Crewlet read one
 * engine's words. The row's status folds the identity in on the engine too.
 */

import {
  AGENT_IDENTITY_STATES,
  AgentIdentity,
  AgentIdentitySheet,
  Callout,
  DescriptionList,
  Text,
  type AgentIdentityBrought,
  type AgentIdentityEntry,
  type AgentIdentityHandIn,
  type AgentIdentityStepNote,
  type AgentIdentityVerdict,
} from "@crewlethq/ui";
import type {
  AccessGuideBack,
  AccessIdentity,
  AccessIdentityHolds,
  AccessIdentityGuide,
  AccessSeat,
} from "~/contract/access.ts";
import { useEffect, useState } from "react";
import type { SetupVerdict, SetupVerdictStep } from "~/contract/setup.ts";
import { Modal } from "~/components/Modal.tsx";
import { VendorWords } from "~/ui/VendorWords.tsx";
import type { IdentityRequest } from "./api.ts";

/** A vendor's ID read from the file's name: Apple's `AuthKey_<KEY ID>.p8`. */
const ID_IN_FILE_NAME = /_([A-Z0-9]{6,})(?: \(\d+\))?\.[A-Za-z0-9]+$/;

export interface IdentityBoxProps {
  seat: AccessSeat;
  /** The integration in the reader's words, where the engine sent no guide. */
  vendor: string;
  identity: AccessIdentity;
  /** A check of this agent's identity is running. */
  checking: boolean;
  /** Why nothing here may change now (held, read-only, syncing, an answer pending), or undefined. */
  held: string | undefined;
  onSetUp: () => void;
  onUseCompany: () => void;
}

/** The identity line at the top of an agent's box, its one action at the top right. */
export function IdentityBox({
  seat,
  vendor,
  identity,
  checking,
  held,
  onSetUp,
  onUseCompany,
}: IdentityBoxProps) {
  const acts = identity.offered && (identity.set_up === true || identity.replace === true);
  // A STATUS THIS BUNDLE DOES NOT KNOW is drawn as needing a person, its own
  // word leading the line, rather than taking the tab down.
  const known = (AGENT_IDENTITY_STATES as readonly string[]).includes(identity.status);
  return (
    <AgentIdentity
      vendor={identity.guide?.vendor ?? vendor}
      agent={seat.name}
      state={checking ? "checking" : known ? identity.status : "none"}
      perAgent={identity.offered}
      summary={known ? identity.said : `${identity.status_label}. ${identity.said}`}
      problem={identity.problem}
      onSetUp={acts ? onSetUp : undefined}
      onUseCompany={identity.use_company ? onUseCompany : undefined}
      disabledReason={held}
    />
  );
}

/** What the person brings back, in the kit's terms. */
export function handInOf(back: AccessGuideBack): AgentIdentityHandIn {
  switch (back.kind) {
    case "file":
      return {
        kind: "file",
        accept: back.accept ?? "",
        hint: <VendorWords text={back.hint} />,
        ...(back.label ? { field: { label: back.label, fromName: ID_IN_FILE_NAME } } : {}),
        // DIGITS ONLY TODAY (Apple's vendor number), so a phone offers its number pad.
        ...(back.detail
          ? { detail: { label: back.detail.label, hint: back.detail.hint, inputMode: "numeric" } }
          : {}),
      };
    case "token":
      return {
        kind: "token",
        label: back.label ?? "Token",
        hint: back.hint ? <VendorWords text={back.hint} /> : undefined,
      };
    default:
      return { kind: "nothing", text: <VendorWords text={back.text} /> };
  }
}

/** What a sheet's check sends: made by Crewlet, a file, a token, or an identity added at the product. */
export async function identityRequest(brought: AgentIdentityBrought): Promise<IdentityRequest> {
  if (brought.made) return { way: "made" };
  if (brought.file) {
    return {
      way: "handed_in",
      value: await brought.file.text(),
      file_name: brought.file.name,
      ...(brought.field ? { id: brought.field } : {}),
      ...(brought.detail ? { detail: brought.detail } : {}),
    };
  }
  if (brought.token) return { way: "handed_in", value: brought.token };
  return { way: "invited" };
}

/**
 * The engine's verdict as the sheet draws it: the sentence, then what each
 * read found; a refused check marks the read it stopped at, with the reason
 * and the vendor's answers under each read that ran.
 */
export function verdictOf(verdict: SetupVerdict): AgentIdentityVerdict {
  const steps: Record<string, AgentIdentityStepNote> = {};
  for (const s of verdict.steps ?? []) steps[s.id] = stepNote(s);
  const ok = verdict.outcome === "works";
  return {
    ok,
    ...(verdict.failed_at ? { failedAt: verdict.failed_at } : {}),
    ...(verdict.steps?.length ? { steps } : {}),
    // WHAT HAPPENED, THE FACTS, THEN WHAT TO DO: each read is said on its own
    // step above, so the verdict repeats none of them.
    view: (
      <div className="agent-access-verdict">
        <Text as="p" className="agent-access-verdict__headline">
          {verdict.summary}
        </Text>
        {(verdict.facts?.length ?? 0) > 0 && (
          <DescriptionList
            dense
            fit
            items={verdict.facts!.map((f) => [
              f.label,
              f.identifier ? <Text mono>{f.value}</Text> : f.value,
            ])}
          />
        )}
        {verdict.warnings?.map((w) => (
          <Callout key={w.action} variant={ok ? "info" : "warning"} title={w.title ?? "Next step"}>
            {w.action}
          </Callout>
        ))}
      </div>
    ),
  };
}

/** What one read found, as the sheet draws it under that read. */
function stepNote(s: SetupVerdictStep): AgentIdentityStepNote {
  return {
    ...(s.said ? { note: s.said } : {}),
    ...(s.detail ? { detail: s.detail } : {}),
  };
}

/** An agent that signs as its own identity now: a new one replaces it. */
const OWN: AccessIdentityHolds = "own";

export interface IdentitySheetProps {
  seat: AccessSeat;
  identity: AccessIdentity;
  guide: AccessIdentityGuide;
  /** Open at the hand-in: a person replacing a working identity holds the new one already. */
  startAtBack?: boolean;
  onClose: () => void;
  onCheck: (brought: AgentIdentityBrought) => Promise<AgentIdentityVerdict>;
}

/**
 * Whether a new identity is still settling at the vendor (`ready_at` ahead),
 * drawn again the moment it passes, so its add links open when they work.
 */
export function useSettling(readyAt: string | undefined): boolean {
  const at = readyAt ? Date.parse(readyAt) : Number.NaN;
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!(at > now)) return undefined;
    const timer = setTimeout(() => setNow(Date.now()), at - now + 250);
    return () => clearTimeout(timer);
  }, [at, now]);
  return at > now;
}

/** The guide's entries as the kit's checklist rows: unread ones carry no state. */
export function entriesOf(guide: AccessIdentityGuide): AgentIdentityEntry[] | undefined {
  if (!guide.entries?.length) return undefined;
  return guide.entries.map((e) => ({
    id: e.id,
    name: e.name || e.id,
    ...(e.group ? { group: e.group } : {}),
    ...(e.add_url ? { href: e.add_url } : {}),
    state: e.state || "unknown",
    ...(e.state === "wrong_role" && e.role ? { note: `Holds ${e.role}` } : {}),
  }));
}

/** The three-step sheet, driven by the engine's guide alone. */
export function IdentitySheet({
  seat,
  identity,
  guide,
  startAtBack,
  onClose,
  onCheck,
}: IdentitySheetProps) {
  const settling = useSettling(guide.ready_at);
  const widerLinks = guide.wider?.links ?? [];
  return (
    <AgentIdentitySheet
      open
      onClose={onClose}
      agent={seat.name}
      vendor={guide.vendor}
      why={guide.why}
      replacing={identity.holds === OWN && identity.replace === true}
      steps={guide.steps.map((s) => ({
        text: <VendorWords text={s.text} />,
        ...(s.copy ? { copy: s.copy } : {}),
      }))}
      entries={entriesOf(guide)}
      entryAction={`Open in ${guide.vendor}`}
      wider={
        guide.wider && widerLinks.length > 0
          ? {
              title: guide.wider.label,
              body: <VendorWords text={guide.wider.does} />,
              links: widerLinks.map((l) => ({ label: l.label, href: l.url })),
            }
          : undefined
      }
      notReady={
        settling
          ? `Ready to add in about a minute: Google is still taking in ${seat.name}'s new identity, and ${guide.vendor} refuses it until then.`
          : undefined
      }
      link={guide.link ? { label: guide.link.label, href: guide.link.url } : undefined}
      note={guide.note ? <VendorWords text={guide.note} /> : undefined}
      caution={identity.blocked}
      make={
        guide.make
          ? {
              title: guide.make.label,
              body: <VendorWords text={guide.make.does} />,
              disabledReason: guide.make.unavailable,
              reads: guide.make.reads,
            }
          : undefined
      }
      handIn={handInOf(guide.back)}
      reads={guide.checks}
      onCheck={onCheck}
      startAt={startAtBack ? "back" : undefined}
      modal={Modal}
    />
  );
}
