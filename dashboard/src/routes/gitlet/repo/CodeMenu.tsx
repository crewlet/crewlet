/**
 * Code: how a repository is cloned, at the address the engine answers (`clone_url`),
 * its whole `git clone` command, and a ZIP of the commit on show.
 */
import { Button, ButtonLink, Copyable, InlineCode, Popover, useClipboard } from "@crewlethq/ui";
import {
  ArrowDownToLineGlyph,
  CheckGlyph,
  ChevronDownGlyph,
  CodeGlyph,
  SquareTerminalGlyph,
} from "@crewlethq/icons/glyphs";
import { shortOID } from "~/lib/code.ts";
import type { CodeRepoDetail } from "~/protocol/index.ts";
import { archiveHref, useDownloadsServed } from "./downloads.ts";

/** Said where the deployment names no address of its own. */
export const NO_CLONE_ADDRESS =
  "This deployment names no address of its own, so the address git clones from is not known here.";
const SET_IT = "Its operator sets it as api.external_url.";

/** The address, or the sentence that says why there is none. */
export function CloneAddress({ url }: { url: string | undefined }) {
  return url ? (
    <Copyable
      className="code-clone__url"
      value={url}
      variant="block"
      monospace
      ariaLabel="Copy the clone address"
      title="Copy the address"
    />
  ) : (
    <p className="code-clone__missing">{`${NO_CLONE_ADDRESS} ${SET_IT}`}</p>
  );
}

/** Copy the clone command: the whole `git clone`, held where there is no address. */
function CopyCommand({ url }: { url: string | undefined }) {
  const { state, copy } = useClipboard({ failedResetMs: null });
  return (
    <Button
      size="small"
      variant="ghost"
      className="code-clone__act"
      block
      leadingIcon={
        state === "copied" ? <CheckGlyph size="sm" /> : <SquareTerminalGlyph size="sm" />
      }
      disabledReason={url ? undefined : NO_CLONE_ADDRESS}
      onClick={() => {
        if (url) void copy(`git clone ${url}`);
      }}
    >
      {state === "copied"
        ? "Copied the clone command"
        : state === "failed"
          ? "The browser refused the clipboard"
          : "Copy the clone command"}
    </Button>
  );
}

/** Download ZIP of the listing's commit, held while it is unknown or where the fleet serves none. */
function DownloadZip({ name, commit, failed }: { name: string; commit: string; failed: boolean }) {
  const served = useDownloadsServed(name, true);
  const held =
    commit === ""
      ? failed
        ? "The commit could not be read, so there is nothing to download yet."
        : "Waiting for the commit to download."
      : served === "no"
        ? "Downloading a ZIP is not available on this fleet yet."
        : served === "checking"
          ? "Checking whether this fleet serves downloads."
          : null;
  const glyph = <ArrowDownToLineGlyph size="sm" />;
  return held ? (
    <Button
      size="small"
      variant="ghost"
      className="code-clone__act"
      block
      leadingIcon={glyph}
      disabledReason={held}
    >
      Download ZIP
    </Button>
  ) : (
    <ButtonLink
      size="small"
      variant="ghost"
      className="code-clone__act"
      block
      leadingIcon={glyph}
      href={archiveHref(name, commit)}
      download
      title={`Every file of ${shortOID(commit)}, as a ZIP`}
    >
      Download ZIP
    </ButtonLink>
  );
}

export function CodeMenu({
  repo,
  cloneURL,
  commit,
  failed,
}: {
  repo: CodeRepoDetail;
  cloneURL: string | undefined;
  /** The commit the root's listing resolved, whole, or none while it is read. */
  commit: string;
  /** The listing could not be read. */
  failed: boolean;
}) {
  return (
    <Popover
      label={`Clone ${repo.name}`}
      align="end"
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="primary"
          leadingIcon={<CodeGlyph size="sm" />}
          trailingIcon={<ChevronDownGlyph size="sm" />}
          aria-haspopup="dialog"
          aria-expanded={open}
          onClick={toggle}
        >
          Code
        </Button>
      )}
    >
      <div className="code-clone">
        <p className="code-clone__head">Clone</p>
        <p className="code-clone__lab">HTTPS</p>
        <CloneAddress url={cloneURL} />
        {cloneURL && (
          <p className="code-clone__note">
            git asks for a password: give it your own token, which{" "}
            <InlineCode>crewlet iam token -login</InlineCode> mints. To have git never ask, make{" "}
            <InlineCode>crewlet gitlet credential</InlineCode> its credential helper.
          </p>
        )}
        <div className="code-clone__acts">
          <CopyCommand url={cloneURL} />
          <DownloadZip name={repo.name} commit={commit} failed={failed} />
        </div>
      </div>
    </Popover>
  );
}
