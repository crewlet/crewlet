/// <reference types="vite/client" />

/**
 * A `?raw` import is the file's own bytes as a string.
 *
 * Declared here because the markdown fixtures are REAL `.md` files: a document
 * written as markdown is one somebody can read and edit as markdown, and a
 * fixture that exists only inside a template literal in a test is one nobody
 * ever looks at again.
 */
declare module "*.md?raw" {
  const content: string;
  export default content;
}
