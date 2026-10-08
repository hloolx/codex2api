import assert from "node:assert/strict"
import { describe, it } from "node:test"
import {
  getGrokImportJob,
  resetGrokImportJobForTests,
  startGrokImportJob,
} from "./grokImportJob.ts"

describe("startGrokImportJob", () => {
  it("keeps running after the caller stops listening and records a later failure", async () => {
    resetGrokImportJobForTests()
    let releaseSecond
    const second = new Promise((resolve) => {
      releaseSecond = resolve
    })
    const updates = []
    const stop = (await import("./grokImportJob.ts")).subscribeGrokImportJob((job) => {
      if (job) updates.push(job.current)
    })
    const task = startGrokImportJob({
      title: "导入 Grok 账号",
      totalItems: 3,
      source: "pool",
      chunks: [
        async () => ({ total: 2, imported: 2, failed: 0, items: [] }),
        async () => {
          stop()
          await second
          throw new Error("单次最多导入 50 个 sso token")
        },
      ],
    })
    await new Promise((resolve) => setTimeout(resolve, 0))
    assert.equal(getGrokImportJob()?.running, true)
    assert.equal(getGrokImportJob()?.current, 2)
    releaseSecond()
    await task
    const done = getGrokImportJob()
    assert.equal(done?.running, false)
    assert.equal(done?.done, true)
    assert.equal(done?.success, 2)
    assert.equal(done?.error, "单次最多导入 50 个 sso token")
    assert.ok(updates.includes(2))
    resetGrokImportJobForTests()
  })
})
