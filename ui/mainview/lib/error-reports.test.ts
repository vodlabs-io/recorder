import { describe, expect, test } from "bun:test"
import type { ErrorEvent } from "@sentry/react"
import { fromInjectedScript } from "./error-reports"

function event(...filenames: (string | undefined)[]): ErrorEvent {
  return {
    type: undefined,
    exception: { values: [{ type: "TypeError", stacktrace: { frames: filenames.map((filename) => ({ filename })) } }] },
  }
}

describe("fromInjectedScript", () => {
  test("drops an error thrown only by script with no source URL", () => {
    expect(fromInjectedScript(event("<anonymous>", "<anonymous>", undefined))).toBe(true)
  })
  test("keeps an error with a frame in the UI's own bundle", () => {
    expect(fromInjectedScript(event("<anonymous>", "views://mainview/assets/index-abc.js"))).toBe(false)
  })
  test("keeps an error with no stack at all", () => {
    expect(fromInjectedScript(event())).toBe(false)
    expect(fromInjectedScript({ type: undefined })).toBe(false)
  })
})
