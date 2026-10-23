import { describe, expect, test } from "bun:test";
import { hostAllowed, storageCookieScript } from "./platform-login";

// hostAllowed is the capture-side half of the cookie guard: it decides which of
// a login partition's cookies are stored at all. It matters most where a
// sign-in spans two registrable domains, because the stored jar is later
// replayed WHOLE to every host the descriptor allows.
describe("hostAllowed", () => {
  test("keeps the platform's own domain and its subdomains", () => {
    const d = ["tiktok.com"];
    for (const host of [".tiktok.com", "tiktok.com", "www.tiktok.com", "WEBCAST.TikTok.com"]) {
      expect(hostAllowed(d, host)).toBe(true);
    }
  });

  test("rejects lookalikes and unrelated hosts", () => {
    const d = ["tiktok.com"];
    for (const host of ["eviltiktok.com", "tiktok.com.evil.com", "example.com"]) {
      expect(hostAllowed(d, host)).toBe(false);
    }
  });

  test("keeps youtube.com cookies but not the Google ones signed in alongside them", () => {
    const d = ["youtube.com"];
    expect(hostAllowed(d, ".youtube.com")).toBe(true);
    expect(hostAllowed(d, "www.youtube.com")).toBe(true);
    // The YouTube sign-in runs on accounts.google.com and leaves its own
    // cookies in the same partition. They authenticate the whole Google
    // account and must never be stored as, or sent as, a YouTube login.
    expect(hostAllowed(d, ".google.com")).toBe(false);
    expect(hostAllowed(d, "accounts.google.com")).toBe(false);
  });

  test("keeps bigo.tv cookies but not the Facebook or Google ones it signs in through", () => {
    const d = ["bigo.tv"];
    // ta.bigo.tv answers the room lookup the login is spent on.
    expect(hostAllowed(d, ".bigo.tv")).toBe(true);
    expect(hostAllowed(d, "ta.bigo.tv")).toBe(true);
    // BIGO's sign-in page offers Facebook and Google alongside its own phone
    // and app flows, so those providers' cookies land in the same partition.
    // They authenticate the whole Facebook/Google account, not a BIGO session.
    expect(hostAllowed(d, ".facebook.com")).toBe(false);
    expect(hostAllowed(d, "accounts.google.com")).toBe(false);
    // The media CDN is authorized by the signed URL the lookup returns.
    expect(hostAllowed(d, "cubetecn.com")).toBe(false);
    expect(hostAllowed(d, "bigovideo.tv")).toBe(false);
  });

  test("drops a cookie whose domain cannot be read", () => {
    // Unattributable: the jar is not per-host, so there is no way to know it
    // belongs to the platform rather than to the sign-in provider.
    expect(hostAllowed(["youtube.com"], undefined)).toBe(false);
    expect(hostAllowed(["youtube.com"], "")).toBe(false);
  });

  test("enforces nothing when the descriptor declares no domains", () => {
    expect(hostAllowed(undefined, "anything.example")).toBe(true);
    expect(hostAllowed([], "anything.example")).toBe(true);
  });
});

// storageCookieScript copies a local-storage value (Fansly's session token)
// into a first-party cookie, so the native capture stores it with the jar.
describe("storageCookieScript", () => {
  test("is empty without entries, and skips unsafe cookie names", () => {
    expect(storageCookieScript(undefined)).toBe("");
    expect(storageCookieScript([])).toBe("");
    expect(storageCookieScript([{ key: "k", cookie: "bad name;" }])).toBe("");
  });

  test("names the key, field and cookie it copies", () => {
    const js = storageCookieScript([
      { key: "session_active_session", field: "token", cookie: "vl_session_token" },
    ]);
    expect(js).toContain('"key":"session_active_session"');
    expect(js).toContain('"field":"token"');
    expect(js).toContain('"cookie":"vl_session_token"');
    // It must parse as a script the page can run.
    expect(() => new Function(js)).not.toThrow();
  });
});
