import { describe, expect, test } from "bun:test";
import { streamerChannelURL } from "./channel-url";
import type { PlatformInfo } from "./types";

const platforms: PlatformInfo[] = [
  { name: "twitch", channel_url: "https://www.twitch.tv/{username}" },
  { name: "tiktok", channel_url: "https://www.tiktok.com/@{username}/live" },
  // A platform with no template.
  { name: "nolink" },
];

describe("streamerChannelURL", () => {
  test("substitutes the username into the template", () => {
    expect(streamerChannelURL(platforms, "twitch", "streamer_three")).toBe(
      "https://www.twitch.tv/streamer_three",
    );
    expect(streamerChannelURL(platforms, "tiktok", "alice")).toBe(
      "https://www.tiktok.com/@alice/live",
    );
  });

  test("returns null when there is no template to build from", () => {
    expect(streamerChannelURL(platforms, "nolink", "alice")).toBeNull();
    expect(streamerChannelURL(platforms, "neverseen", "alice")).toBeNull();
    expect(streamerChannelURL([], "twitch", "alice")).toBeNull();
    expect(streamerChannelURL(platforms, "twitch", "")).toBeNull();
  });

  // The template comes from the cloud and the result is opened in the user's
  // real browser.
  test("rejects a template that is not an https URL", () => {
    const bad: PlatformInfo[] = [
      { name: "js", channel_url: "javascript:alert({username})" },
      { name: "plain", channel_url: "http://plain.example/{username}" },
      { name: "file", channel_url: "file:///etc/{username}" },
      { name: "junk", channel_url: "not a url {username}" },
    ];
    for (const p of bad) {
      expect(streamerChannelURL(bad, p.name, "alice")).toBeNull();
    }
  });

  test("escapes a username that would otherwise alter the path", () => {
    expect(streamerChannelURL(platforms, "twitch", "a/../b")).toBe(
      "https://www.twitch.tv/a%2F..%2Fb",
    );
  });
});
