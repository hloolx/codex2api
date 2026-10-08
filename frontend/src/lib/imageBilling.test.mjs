import assert from "node:assert/strict";
import test from "node:test";

import { mediaBillingUnit } from "./imageBilling.ts";

test("mediaBillingUnit classifies Grok Imagine models like the backend", () => {
  assert.equal(mediaBillingUnit("grok-imagine-image"), "image");
  assert.equal(mediaBillingUnit("grok-imagine"), "image");
  assert.equal(mediaBillingUnit("Grok-Imagine-Video-1.5-Preview"), "second");
  assert.equal(mediaBillingUnit("xai/grok-imagine-video"), "second");
  assert.equal(mediaBillingUnit("grok-4.7"), "");
  assert.equal(mediaBillingUnit("gpt-image-2"), "");
});
