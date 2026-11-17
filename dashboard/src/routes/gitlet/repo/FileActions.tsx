/**
 * A file's bytes from its card's head: Raw and Download link the raw route at the
 * commit read, and Copy writes every page, reading those not loaded.
 */
import { useEffect, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  IconButton,
  IconButtonLink,
  Spinner,
  useAnnouncer,
  writeClipboard,
} from "@crewlethq/ui";
import { ArrowDownToLineGlyph, CheckGlyph, CopyGlyph } from "@crewlethq/icons/glyphs";
import { baseName, shortOID } from "~/lib/code.ts";
import { fmtBytes } from "~/lib/format.ts";
import type { CodeBlobAnswer } from "~/protocol/index.ts";
import { readWhole } from "./blobs.ts";
import { rawHref, useDownloadsServed } from "./downloads.ts";

/** Copy reads at most 1 MiB, about nineteen of the engine's pages; Raw serves any size. */
export const COPY_MAX_BYTES = 1 << 20;

/** Said while the read that names the commit is still on its way. */
export const WAITING_FOR_COMMIT = "Waiting for the commit the file was read at.";

/**
 * Write what `read` answers. Where `ClipboardItem` takes a promise the write is made
 * at the press, so a browser that ties the clipboard to it keeps it; a failed read throws.
 */
export async function copyWhole(read: () => Promise<string>): Promise<boolean> {
  const clipboard = typeof navigator === "undefined" ? undefined : navigator.clipboard;
  const Item = typeof ClipboardItem === "undefined" ? undefined : ClipboardItem;
  if (Item?.supports?.("text/plain") && clipboard?.write) {
    const text = read();
    const blob = text.then((t) => new Blob([t], { type: "text/plain" }));
    try {
      await clipboard.write([new Item({ "text/plain": blob })]);
      return true;
    } catch {
      await text;
      return false;
    }
  }
  return writeClipboard(await read());
}

/** Why Copy is held for `file`, or undefined. */
export function copyHeld(file: CodeBlobAnswer): string | undefined {
  if (file.binary) return "A binary file has no text to copy: download it.";
  if (file.size === 0 || file.lines === 0) return "This file is empty.";
  if (file.size > COPY_MAX_BYTES) {
    return `This file is ${fmtBytes(file.size)}: copy one past ${COPY_MAX_BYTES >> 20} MiB from Raw.`;
  }
  return undefined;
}

type CopyState = "idle" | "reading" | "copied" | "failed" | "unread";

const COPY_LABEL: Record<CopyState, string> = {
  idle: "Copy the file's contents",
  reading: "Reading the file to copy it",
  copied: "Copied the file's contents",
  failed: "The browser refused the clipboard",
  unread: "The file could not be read to copy it.",
};

/** Copy the file's contents: the pages loaded where they are the whole file, else every page read. */
export function CopyFileButton({
  repo,
  path,
  commit,
  file,
  loaded,
}: {
  repo: string;
  path: string;
  /** The commit the file was read at, whole, or "" while it is read. */
  commit: string;
  file: CodeBlobAnswer;
  /** The whole text where every page is loaded, else null. */
  loaded: string | null;
}) {
  const [state, setState] = useState<CopyState>("idle");
  const [attempt, setAttempt] = useState(0);
  const announce = useAnnouncer();
  const flight = useRef<AbortController | null>(null);
  useEffect(() => () => flight.current?.abort(), []);
  useEffect(() => {
    if (state !== "copied") return undefined;
    const reset = setTimeout(() => setState("idle"), 2000);
    return () => clearTimeout(reset);
  }, [state, attempt]);
  const held =
    copyHeld(file) ?? (loaded === null && commit === "" ? WAITING_FOR_COMMIT : undefined);
  const press = async () => {
    flight.current?.abort();
    const abort = new AbortController();
    flight.current = abort;
    if (loaded === null) setState("reading");
    let copied: boolean;
    try {
      copied = await copyWhole(() =>
        loaded !== null
          ? Promise.resolve(loaded)
          : readWhole(repo, path, commit, abort.signal, COPY_MAX_BYTES),
      );
    } catch {
      if (abort.signal.aborted) return;
      setState("unread");
      announce(COPY_LABEL.unread);
      return;
    }
    if (abort.signal.aborted) return;
    setState(copied ? "copied" : "failed");
    setAttempt((n) => n + 1);
    announce(COPY_LABEL[copied ? "copied" : "failed"]);
  };
  return (
    <IconButton
      label={COPY_LABEL[state]}
      icon={
        state === "reading" ? (
          <Spinner size="sm" />
        ) : state === "copied" ? (
          <CheckGlyph />
        ) : (
          <CopyGlyph />
        )
      }
      size="sm"
      variant="secondary"
      aria-busy={state === "reading" || undefined}
      disabledReason={held}
      onClick={() => {
        if (!held && state !== "reading") void press();
      }}
    />
  );
}

/** Why Raw and Download are held, or undefined: the commit unknown, or a fleet that serves none. */
function linksHeld(commit: string, served: ReturnType<typeof useDownloadsServed>) {
  if (commit === "") return WAITING_FOR_COMMIT;
  if (served === "no") return "Downloading a file is not available on this fleet yet.";
  if (served === "checking") return "Checking whether this fleet serves downloads.";
  return undefined;
}

/** Raw, then Copy and Download joined: the file's bytes at the commit read. */
export function FileBytes({
  repo,
  path,
  commit,
  file,
  loaded,
}: {
  repo: string;
  path: string;
  commit: string;
  file: CodeBlobAnswer;
  loaded: string | null;
}) {
  const served = useDownloadsServed(repo, true);
  const held = linksHeld(commit, served);
  const name = baseName(path);
  const download = <ArrowDownToLineGlyph />;
  return (
    <>
      {held ? (
        <Button size="small" variant="secondary" disabledReason={held}>
          Raw
        </Button>
      ) : (
        <ButtonLink
          size="small"
          variant="secondary"
          href={rawHref(repo, commit, path)}
          title={
            file.binary
              ? "A binary file downloads rather than shows"
              : `The file's bytes as committed at ${shortOID(commit)}`
          }
        >
          Raw
        </ButtonLink>
      )}
      <span className="code-blob__pair">
        <CopyFileButton repo={repo} path={path} commit={commit} file={file} loaded={loaded} />
        {held ? (
          <IconButton
            label={`Download ${name}`}
            icon={download}
            size="sm"
            variant="secondary"
            disabledReason={held}
          />
        ) : (
          <IconButtonLink
            label={`Download ${name}`}
            icon={download}
            size="sm"
            variant="secondary"
            href={rawHref(repo, commit, path, true)}
            download={name}
          />
        )}
      </span>
    </>
  );
}
