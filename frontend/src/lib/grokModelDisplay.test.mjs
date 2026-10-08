import assert from "node:assert/strict";
import test from "node:test";
import { OAUTH_GROK_PRESET_MODELS, API_KEY_GROK_PRESET_MODELS, grokConnectionTestModels, grokDisplayModels, grokPresetModels } from "./grokModelDisplay.ts";

test("auto model directory follows upstream without modifying the model list", () => {
 const account={models:[],grok_models:{models:["grok-4.6","grok-4.7"],status:"fresh"}};
 const result=grokDisplayModels(account);
 assert.deepEqual(result,["grok-4.6","grok-4.7"]);
 result.push("new");
 assert.deepEqual(account.models,[]);
 assert.equal(account.grok_models.models.length,2);
});
test("explicit model list stays as saved, including a known empty catalog", () => {
 assert.deepEqual(grokDisplayModels({models:["grok-4.6"],grok_models:{models:["grok-4.6","grok-4.7"],status:"fresh"}}),["grok-4.6"]);
 assert.deepEqual(grokDisplayModels({models:["grok-4.6"],grok_models:{models:[],status:"fresh"}}),[]);
 assert.deepEqual(grokDisplayModels({models:["grok-4.6"]}),["grok-4.6"]);
 assert.deepEqual(
  grokDisplayModels({
   grok_auth_kind:"oauth",
   models:["grok-4.5","grok-4.6","grok-4.7","grok-4.7-fast"],
   grok_models:{models:["grok-4.7"],status:"fresh"},
  }),
  ["grok-4.7","grok-4.5","grok-4.6","grok-4.7-fast"],
 );
});
test("connection test models follow the catalog, drop image models and only default when the catalog is unknown", () => {
 assert.deepEqual(grokConnectionTestModels({models:[],grok_models:{models:["grok-4.7","grok-imagine-image"],status:"fresh"}}),["grok-4.7"]);
 assert.deepEqual(grokConnectionTestModels({models:[],grok_models:{models:[],status:"fresh"}}),[]);
 assert.deepEqual(grokConnectionTestModels({models:[]}),OAUTH_GROK_PRESET_MODELS);
 assert.deepEqual(grokConnectionTestModels({models:[],grok_models:{models:[],status:"unknown"}}),OAUTH_GROK_PRESET_MODELS);
 assert.deepEqual(grokConnectionTestModels({models:[],grok_auth_kind:"api_key"}),API_KEY_GROK_PRESET_MODELS);
 assert.deepEqual(grokPresetModels("oauth"),OAUTH_GROK_PRESET_MODELS);
 assert.ok(!OAUTH_GROK_PRESET_MODELS.includes("grok-4"));
 assert.ok(!OAUTH_GROK_PRESET_MODELS.includes("grok-3"));
});
