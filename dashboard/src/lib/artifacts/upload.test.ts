import { afterEach, expect, test, vi } from "vitest";
import { uploadFiles } from "./api.ts";

afterEach(() => vi.unstubAllGlobals());

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function engine(intent: Record<string, unknown>) {
  const sent: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      sent.push({ url, init });
      return init.method === "POST" ? json(intent, 201) : new Response(null, { status: 204 });
    }),
  );
  return sent;
}

// A NEW ARTIFACT'S FILES go up through the create intent, text as text and a
// picture as its bytes, each through the engine's own transfer route.
test("a create's files go through the create intent, text and bytes alike", async () => {
  const sent = engine({
    upload: "u-1",
    expires_at: "",
    transfers: [
      { path: "index.html", method: "PUT", url: "", direct: false },
      { path: "img/logo.png", method: "PUT", url: "", direct: false },
    ],
  });
  const png = new Blob([new Uint8Array([0x89, 0x50])], { type: "image/png" });
  const out = await uploadFiles(
    null,
    [
      { path: "index.html", body: "<p>é</p>" },
      { path: "img/logo.png", body: png },
    ],
    "k-1",
  );
  expect(sent[0]!.url).toContain("/artifacts/uploads");
  const entries = JSON.parse(String(sent[0]!.init.body)).entries;
  expect(entries[0]).toMatchObject({ path: "index.html", size: 9 });
  expect(entries[1]).toMatchObject({ path: "img/logo.png", size: 2 });
  expect(entries[1].sha256).toMatch(/^[0-9a-f]{64}$/);
  expect(sent[1]!.url).toContain("/artifacts/uploads/u-1/files/index.html");
  expect(sent[2]!.url).toContain("/artifacts/uploads/u-1/files/img/logo.png");
  expect(sent[2]!.init.body).toBe(png);
  expect(out).toEqual({
    upload: "u-1",
    files: [
      { path: "index.html", upload: true },
      { path: "img/logo.png", upload: true },
    ],
  });
});

// A FILE THE ARTIFACT HOLDS moves no byte: it is carried by its digest.
test("a held file is carried by its digest", async () => {
  const sent = engine({ upload: "u-2", expires_at: "", carried: ["app.css"] });
  const out = await uploadFiles("a-1", [{ path: "app.css", body: "a{}" }], "k-2");
  expect(sent).toHaveLength(1);
  expect(sent[0]!.url).toContain("/artifacts/a-1/uploads");
  expect(out.files[0]).toEqual({ path: "app.css", carry: expect.stringMatching(/^[0-9a-f]{64}$/) });
});

// A DIRECT TRANSFER IS THE CONTRACT BROKEN, refused rather than followed with
// the reader's credential.
test("a direct transfer is refused", async () => {
  engine({
    upload: "u-3",
    expires_at: "",
    transfers: [{ path: "a.html", method: "PUT", url: "https://bucket", direct: true }],
  });
  await expect(uploadFiles(null, [{ path: "a.html", body: "x" }], "k-3")).rejects.toThrow(/bucket/);
});

// AN ASSET UPLOAD SAYS SO, on the artifact's own intent: the engine decides it
// by the asset verb and holds it to the asset store's bounds. Mutation: drop
// `asset: true` and the intent is read as the next version's.
test("an asset upload goes through the artifact's intent and says it is for the asset store", async () => {
  const sent = engine({
    upload: "u-4",
    expires_at: "",
    transfers: [{ path: "venues.csv", method: "PUT", url: "", direct: false }],
  });
  await uploadFiles("a-1", [{ path: "venues.csv", body: "a,b\n" }], "k-4", "assets");
  expect(sent[0]!.url).toContain("/artifacts/a-1/uploads");
  expect(JSON.parse(String(sent[0]!.init.body))).toMatchObject({
    asset: true,
    entries: [{ path: "venues.csv", size: 4 }],
  });
  expect(sent[1]!.url).toContain("/artifacts/uploads/u-4/files/venues.csv");
});

test("a version's upload says nothing of assets", async () => {
  const sent = engine({ upload: "u-5", expires_at: "", transfers: [] });
  await uploadFiles("a-1", [{ path: "index.html", body: "<p>" }], "k-5");
  expect(JSON.parse(String(sent[0]!.init.body))).not.toHaveProperty("asset");
});
