import { describe, expect, test } from "bun:test";
import { scrubEvent, scrubString } from "./report-scrub";

describe("scrubString", () => {
  test("removes the user's name from home-folder paths", () => {
    expect(scrubString("open /Users/jane/Downloads/a.flv")).toBe("open /Users/~/Downloads/a.flv");
    expect(scrubString("/home/jane/.cache")).toBe("/home/~/.cache");
    expect(scrubString(String.raw`C:\Users\Jane\Videos`)).toBe(String.raw`C:\Users\~\Videos`);
    expect(scrubString(String.raw`"C:\\Users\\Jane\\Videos"`)).toBe(String.raw`"C:\\Users\\~\\Videos"`);
  });

  test("removes query strings, emails and JWTs", () => {
    expect(scrubString("GET https://cdn.example/live.m3u8?sig=abc failed")).toBe(
      "GET https://cdn.example/live.m3u8?[removed] failed",
    );
    expect(scrubString("at /link?code=ABCD")).toBe("at /link");
    expect(scrubString("jane@example.com eyJa.eyJb.c")).toBe("[email] [removed]");
  });
});

test("scrubEvent drops the computer's name and keeps the IP", () => {
  const ev = scrubEvent({
    server_name: "Janes-MacBook",
    user: { id: "x", ip_address: "{{auto}}" },
    exception: { values: [{ value: "ENOENT /Users/jane/a", stacktrace: { frames: [{ filename: "/Users/jane/app.js" }] } }] },
    contexts: { log: { token: "secret", path: "/Users/jane/b" } },
  });
  expect(ev.server_name).toBeUndefined();
  expect(ev.user as object).toEqual({ ip_address: "{{auto}}" });
  expect(ev.exception?.values?.[0].value).toBe("ENOENT /Users/~/a");
  expect(ev.exception?.values?.[0].stacktrace?.frames?.[0].filename).toBe("/Users/~/app.js");
  expect(ev.contexts).toEqual({ log: { token: "[removed]", path: "/Users/~/b" } });
});
