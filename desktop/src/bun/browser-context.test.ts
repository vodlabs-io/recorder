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

  test("is script a JavaScript engine can parse", () => {
    const js = buildInjectedScript("id", "https://a/x", "GET", "", "(a)(b)", true, { "X-A": "1" });
    expect(() => new Function(js)).not.toThrow();
  });
});
