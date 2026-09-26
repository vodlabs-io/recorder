// Downloads a Demo dataset's images, which are kept out of git: images.lock
// lists each file under images/ with its SHA-256, and the file is stored in a
// private Backblaze B2 bucket as <prefix>/<sha256>.jpg. Files already present
// with the right hash are skipped.
//
//   bun tools/showcase/fetch-images.ts [--dataset DIR] [--key-file PATH]
//
// Credentials come from B2_APPLICATION_KEY_ID and B2_APPLICATION_KEY, or from
// a --key-file of KEY=VALUE lines; they are never printed. Without them the
// script exits 0 after a warning: the Shots still render, with initials for
// avatars and placeholders for thumbnails.

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { parseArgs } from "node:util";

const here = dirname(new URL(import.meta.url).pathname);

export const BUCKET = "vodlabs-images-private";
export const PREFIX = "showcase";

export type Lock = { path: string; sha256: string }[];

export function readLock(dataset: string): Lock {
  const text = readFileSync(join(dataset, "images.lock"), "utf8");
  return text
    .split("\n")
    .filter((l) => l && !l.startsWith("#"))
    .map((l) => {
      const [sha256, path] = l.split(/\s+/);
      return { path, sha256 };
    });
}

export function sha256(path: string): string {
  return new Bun.CryptoHasher("sha256").update(readFileSync(path)).digest("hex");
}

export function credentials(keyFile?: string): { id: string; key: string } | null {
  const env: Record<string, string | undefined> = { ...process.env };
  if (keyFile) {
    for (const line of readFileSync(keyFile, "utf8").split("\n")) {
      const i = line.indexOf("=");
      if (i > 0) env[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
  }
  const id = env.B2_APPLICATION_KEY_ID;
  const key = env.B2_APPLICATION_KEY;
  return id && key ? { id, key } : null;
}

export type B2Auth = { accountId: string; apiUrl: string; downloadUrl: string; authorizationToken: string; bucketId?: string };

export async function authorize(c: { id: string; key: string }): Promise<B2Auth> {
  const res = await fetch("https://api.backblazeb2.com/b2api/v3/b2_authorize_account", {
    headers: { Authorization: "Basic " + btoa(`${c.id}:${c.key}`) },
  });
  if (!res.ok) throw new Error(`B2 authorization failed: ${res.status}`);
  const body = (await res.json()) as any;
  const api = body.apiInfo.storageApi;
  return {
    accountId: body.accountId,
    apiUrl: api.apiUrl,
    downloadUrl: api.downloadUrl,
    authorizationToken: body.authorizationToken,
    bucketId: api.allowed?.buckets?.find((b: any) => b.name === BUCKET)?.id,
  };
}

async function main(): Promise<void> {
  const { values: args } = parseArgs({
    options: {
      dataset: { type: "string", default: join(here, "datasets/default") },
      "key-file": { type: "string" },
    },
  });
  const dataset = args.dataset!;
  const lock = readLock(dataset);
  const missing = lock.filter((f) => {
    const p = join(dataset, f.path);
    return !existsSync(p) || sha256(p) !== f.sha256;
  });
  if (!missing.length) {
    console.log(`${lock.length} images up to date`);
    return;
  }
  const c = credentials(args["key-file"]);
  if (!c) {
    console.warn(`${missing.length} images missing and no B2 credentials; Shots will show placeholders.`);
    return;
  }
  const auth = await authorize(c);
  let failed = 0;
  let i = 0;
  await Promise.all(
    Array.from({ length: 8 }, async () => {
      while (i < missing.length) {
        const f = missing[i++];
        const res = await fetch(`${auth.downloadUrl}/file/${BUCKET}/${PREFIX}/${f.sha256}.jpg`, {
          headers: { Authorization: auth.authorizationToken },
        });
        const bytes = new Uint8Array(await res.arrayBuffer());
        const got = new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
        if (!res.ok || got !== f.sha256) {
          console.error(`${f.path}: ${res.ok ? "hash mismatch" : res.status}`);
          failed++;
          continue;
        }
        mkdirSync(dirname(join(dataset, f.path)), { recursive: true });
        writeFileSync(join(dataset, f.path), bytes);
      }
    }),
  );
  console.log(`fetched ${missing.length - failed} of ${missing.length} images`);
  if (failed) process.exit(1);
}

if (import.meta.main) await main();
