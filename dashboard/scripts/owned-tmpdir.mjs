// Runs a command inside a temporary directory this script owns, and removes
// that directory when the command ends, however it ends.
//
// IT EXISTS BECAUSE VITEST LEAKS ONE DIRECTORY PER RUN. The forks pool hands
// every transformed module to its workers as a file under
// `join(os.tmpdir(), nanoid())`, and for the root project that directory is
// the Vitest instance's own (`project._fetcher = vitest._fetcher`), which
// nothing removes: `TestProject.close()` clears the PROJECT's directory, a
// different one the root project never writes. Measured on 5.0.1 and 5.0.2:
// every `vitest run` leaves 16-48 MB behind, and a working day of gate runs
// had left 696 of them, 11.9 GB, on the machine that runs the engine's own
// suites — whose embedded broker derives its byte ceiling from the free space
// on the volume, so the Go suites then failed with "insufficient storage
// resources available" for a reason nothing in their output pointed at.
//
// The fix is to own the directory rather than to find Vitest's in it: POSIX
// `os.tmpdir()` reads TMPDIR, so the child's every temporary file lands under
// one directory created here, and removing that removes all of them. Reaching
// for `vitest._tmpDir` from a global setup would name an internal field whose
// rename turns the cleanup into a silent no-op; TMPDIR is the documented
// contract of `os.tmpdir()` itself.
//
// Usage: node scripts/owned-tmpdir.mjs <command> [args...]
// The command's exit status (or the signal that ended it) is this script's.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [command, ...args] = process.argv.slice(2);
if (!command) {
  process.stderr.write("usage: node scripts/owned-tmpdir.mjs <command> [args...]\n");
  process.exit(2);
}

const owned = mkdtempSync(join(tmpdir(), "crewlet-dashboard-"));
const remove = () => rmSync(owned, { recursive: true, force: true });

const child = spawn(command, args, {
  stdio: "inherit",
  env: { ...process.env, TMPDIR: owned, TMP: owned, TEMP: owned },
});

// A Ctrl-C reaches the whole foreground process group, the child included, so
// the child ends on its own; forwarding TERM covers a runner that signals
// only this process. Either way the removal waits for the child to exit,
// because removing the directory under a live worker fails its reads.
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(signal, () => child.kill(signal));
}

child.on("error", (err) => {
  remove();
  process.stderr.write(`owned-tmpdir: cannot run ${command}: ${err.message}\n`);
  process.exit(127);
});

child.on("exit", (code, signal) => {
  remove();
  if (signal) {
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
