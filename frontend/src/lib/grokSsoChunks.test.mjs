import assert from "node:assert/strict"
import { describe, it } from "node:test"
import { GROK_SSO_IMPORT_CHUNK_SIZE, splitGrokSSOImport } from "./grokSsoChunks.ts"

describe("splitGrokSSOImport", () => {
  it("splits plain text into batches of 50 and skips blanks", () => {
    const lines = Array.from({ length: 120 }, (_, i) => `token-${i + 1}`)
    lines.splice(10, 0, "", "# comment", "   ")
    const split = splitGrokSSOImport(lines.join("\n"))
    assert.equal(split.total, 120)
    assert.equal(split.payloads.length, 3)
    assert.equal(split.payloads[0].split("\n").length, GROK_SSO_IMPORT_CHUNK_SIZE)
    assert.equal(split.payloads[2].split("\n").length, 20)
    assert.equal(split.payloads[0].split("\n")[0], "token-1")
  })

  it("splits a JSON account list without exceeding the batch size", () => {
    const accounts = Array.from({ length: 51 }, (_, i) => ({
      email: `u${i}@example.com`,
      sso_token: `sso-${i}`,
    }))
    const split = splitGrokSSOImport(JSON.stringify({ accounts }))
    assert.equal(split.total, 51)
    assert.equal(split.payloads.length, 2)
    assert.equal(JSON.parse(split.payloads[0]).accounts.length, 50)
    assert.equal(JSON.parse(split.payloads[1]).accounts.length, 1)
  })

  it("rejects malformed JSON instead of treating it as lines", () => {
    assert.throws(() => splitGrokSSOImport("{not json"), /JSON/)
  })

  it("returns an empty split for blank input", () => {
    assert.deepEqual(splitGrokSSOImport(" \n# only\n "), { total: 0, payloads: [] })
  })
})
