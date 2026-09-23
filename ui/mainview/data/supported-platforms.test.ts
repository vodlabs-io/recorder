import { describe, expect, test } from "bun:test";
import {
  platformDisplayName,
  registerPlatforms,
  streamerChannelURL,
} from "./supported-platforms";

// The registry wiring only: the template rules themselves are covered in
// shared/channel-url.test.ts.
describe("registerPlatforms", () => {
  test("nothing resolves before the list has loaded", () => {
    expect(streamerChannelURL("twitch", "streamer_three")).toBeNull();
    // A label always resolves, from the built-in table.
    expect(platformDisplayName("twitch")).toBe("Twitch");
  });

  test("a registered list feeds both lookups", () => {
    registerPlatforms([
      {
        name: "twitch",
        display_name: "Twitch!",
        channel_url: "https://www.twitch.tv/{username}",
      },
    ]);
    expect(streamerChannelURL("twitch", "streamer_three")).toBe(
      "https://www.twitch.tv/streamer_three",
    );
    expect(platformDisplayName("twitch")).toBe("Twitch!");
  });

  test("a later list replaces the templates rather than merging them", () => {
    registerPlatforms([{ name: "kick", channel_url: "https://kick.com/{username}" }]);
    expect(streamerChannelURL("kick", "streamer_one")).toBe("https://kick.com/streamer_one");
    expect(streamerChannelURL("twitch", "streamer_three")).toBeNull();
  });
});
