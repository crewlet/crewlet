// The NOTICE files a bundle's packages ship, carried in the notices file beside
// it by every build that writes one (vite.config.ts, vite.runtime.config.ts).
import type { Plugin } from "vite";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// packageNotices is the NOTICE file of each package build.license listed in
// `listed`: an Apache-2.0 package's license requires a redistribution to carry
// it, and build.license writes the LICENSE alone.
export function packageNotices(listed: string): string {
  let text = "";
  for (const [, name] of listed.matchAll(/^## (\S+) - /gm)) {
    const file = fileURLToPath(new URL(`./node_modules/${name}/NOTICE`, import.meta.url));
    if (existsSync(file)) text += `\n\n## ${name}: NOTICE\n\n${readFileSync(file, "utf-8").trim()}`;
  }
  return text;
}

// withPackageNotices appends [packageNotices] to the notices asset `fileName`,
// after build.license has written it (see sourceNotices for `order: "post"`).
export function withPackageNotices(fileName: string): Plugin {
  return {
    name: "crewlet:package-notices",
    apply: "build",
    generateBundle: {
      order: "post",
      handler(_options, bundle) {
        const notices = bundle[fileName];
        if (notices?.type !== "asset") {
          this.error(`${fileName} was not emitted, so build.license no longer writes it.`);
        }
        const text = String(notices.source).trimEnd();
        notices.source = `${text}${packageNotices(text)}\n`;
      },
    },
  };
}
