import { describe, expect, test } from "bun:test"
import { sessionExpired, webUrls } from "./web-client"

describe("webUrls", () => {
  test("resolves against the page at the root", () => {
    const u = webUrls("http://192.168.1.5:8080/#/settings/files")
    expect(u.nats).toBe("ws://192.168.1.5:8080/nats")
    expect(u.session).toBe("http://192.168.1.5:8080/api/session")
    expect(u.signin).toBe("http://192.168.1.5:8080/signin")
  })

  test("stays under a reverse proxy's subpath, over TLS", () => {
    const u = webUrls("https://home.example/recorder/#/")
    expect(u.nats).toBe("wss://home.example/recorder/nats")
    expect(u.signout).toBe("https://home.example/recorder/api/signout")
    expect(u.signin).toBe("https://home.example/recorder/signin")
  })
})

describe("media URLs", () => {
  test("encode the folder and file, and stay under the subpath", () => {
    const u = webUrls("https://home.example/recorder/")
    expect(u.media("/recordings/kick/a b/2026-10-01", "vod #1.mp4")).toBe(
      "https://home.example/recorder/api/vods/file?dir=%2Frecordings%2Fkick%2Fa+b%2F2026-10-01&file=vod+%231.mp4",
    )
    expect(u.media("/r", "vod.mp4", true)).toBe(
      "https://home.example/recorder/api/vods/file?dir=%2Fr&file=vod.mp4&download=1",
    )
  })
})

describe("sessionExpired", () => {
  const answer = (status: number) =>
    (async () => new Response(null, { status })) as unknown as typeof fetch

  test("only a 401 means signed out", async () => {
    expect(await sessionExpired("/api/session", answer(401))).toBe(true)
    expect(await sessionExpired("/api/session", answer(204))).toBe(false)
    expect(await sessionExpired("/api/session", answer(502))).toBe(false)
  })

  test("an unreachable daemon is not a sign-out", async () => {
    const down = (async () => {
      throw new TypeError("network")
    }) as unknown as typeof fetch
    expect(await sessionExpired("/api/session", down)).toBe(false)
  })
})
