import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
  acquireSingleInstanceLock,
  releaseSingleInstanceLock,
} from "./single-instance";

// Use a fixed test port so the test never collides with a running app.
const TEST_PORT = "47616";

beforeEach(() => {
  process.env.VLREC_APP_LOCK_PORT = TEST_PORT;
});

afterEach(() => {
  releaseSingleInstanceLock();
  delete process.env.VLREC_APP_LOCK_PORT;
});

// waitFor polls predicate until true or it times out, so the test doesn't depend
// on exact loopback round-trip timing.
async function waitFor(predicate: () => boolean, ms = 1000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!predicate() && Date.now() < deadline) {
    await Bun.sleep(10);
  }
}

describe("acquireSingleInstanceLock", () => {
  test("first caller becomes primary", async () => {
    expect(await acquireSingleInstanceLock(() => {})).toBe(true);
  });

  test("second caller is rejected and activates the primary", async () => {
    let activated = 0;
    expect(await acquireSingleInstanceLock(() => activated++)).toBe(true);

    expect(await acquireSingleInstanceLock(() => {})).toBe(false);

    await waitFor(() => activated > 0);
    expect(activated).toBe(1);
  });

  test("lock is reusable once released", async () => {
    expect(await acquireSingleInstanceLock(() => {})).toBe(true);
    releaseSingleInstanceLock();
    expect(await acquireSingleInstanceLock(() => {})).toBe(true);
  });
});
