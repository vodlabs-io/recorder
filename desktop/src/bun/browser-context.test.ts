import { describe, expect, test } from "bun:test";
import { buildInjectedScript, fetchInit, pageURLFor } from "./browser-context";

describe("pageURLFor", () => {
  test("defaults to the origin's front page", () => {
    expect(pageURLFor("https://www.tiktok.com/api/x?y=1")).toBe("https://www.tiktok.com/");
  });

  test("names a page of the request's own origin", () => {
    expect(pageURLFor("https://www.instagram.com/api/x", "/robots.txt")).toBe(
      "https://www.instagram.com/robots.txt",
    );
  });

  test("a host origin hosts the page for its own subdomain's requests", () => {
    expect(pageURLFor("https://api.rplay.live/live/x", "/p/streams", "https://rplay.live")).toBe(
      "https://rplay.live/p/streams",
    );
  });

  test("a host origin that is not a parent of the request's host is ignored", () => {
    for (const origin of ["https://other.live", "http://rplay.live", "https://api.rplay.live.evil.test"]) {
      expect(pageURLFor("https://api.rplay.live/x", "/p", origin)).toBe("https://api.rplay.live/p");
    }
    expect(pageURLFor("https://api.rplay.live/x", undefined, "https://rplay.live")).toBe(
      "https://api.rplay.live/",
    );
  });

  test("a host page can never leave the origin it is relative to by its path", () => {
    expect(new URL(pageURLFor("https://a.example/x", "robots.txt")).host).toBe("a.example");
  });
});

describe("fetchInit", () => {
  test("sends the request's own headers and leaves the browser's to the browser", () => {
    const init = fetchInit("GET", "", {
      "X-IG-App-ID": "1",
      Referer: "https://www.instagram.com/",
      "User-Agent": "ua",
      Cookie: "a=b",
    });
    expect(init).toEqual({
      credentials: "include",
      method: "GET",
      referrer: "https://www.instagram.com/",
      headers: { "X-IG-App-ID": "1" },
    });
  });

  test("a request to another origin than the page's sends no cookies", () => {
    expect(fetchInit("GET", "", undefined, true).credentials).toBe("same-origin");
  });

  test("a request with no headers sets none", () => {
    expect(fetchInit("POST", "body", undefined)).toEqual({
      credentials: "include",
      method: "POST",
      body: "body",
    });
  });
});

describe("buildInjectedScript", () => {
  test("waits for the signing SDK on a signed page only", () => {
    expect(buildInjectedScript("id", "https://a/x", "GET", "", "")).toContain("i < 60");
    expect(buildInjectedScript("id", "https://a/x", "GET", "", "", true)).toContain("i < 0");
  });

  test("reports where a redirect ended up", () => {
    expect(buildInjectedScript("id", "https://a/x", "GET", "", "")).toContain("redirected_to");
  });

  test("reads a stored header's value in the page and sends it", () => {
    const js = buildInjectedScript("id", "https://a/x", "GET", "", "", true, undefined, true, {
      header: "x-browser-pass",
      database: "db",
      store: "records",
      key: "pass",
      field: "pass",
      expires_field: "renewAt",
      wait_ms: 1000,
    });
    expect(js).toContain('"x-browser-pass"');
    expect(js).toContain("1000");
    expect(() => new Function(js)).not.toThrow();
  });

  test("never sets one of the browser's own headers from storage", () => {
    const js = buildInjectedScript("id", "https://a/x", "GET", "", "", true, undefined, false, {
      header: "Cookie",
      database: "db",
      store: "s",
      key: "k",
      field: "f",
    });
    expect(js).toContain("var SH = null;");
  });

  test("is script a JavaScript engine can parse", () => {
    const js = buildInjectedScript("id", "https://a/x", "GET", "", "(a)(b)", true, { "X-A": "1" });
    expect(() => new Function(js)).not.toThrow();
  });
});
