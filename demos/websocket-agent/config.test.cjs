const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const demoRoot = __dirname;

test("demo runtime config starts only the default-agent WebSocket entrypoint", () => {
  const runtimeConfig = fs.readFileSync(path.join(demoRoot, "xira.yaml"), "utf8");
  const entrypointsConfig = fs.readFileSync(
    path.join(demoRoot, "entrypoints.yaml"),
    "utf8",
  );

  assert.match(runtimeConfig, /^workspace:\s+\.\.\/\.\.\/workspace$/m);
  assert.match(runtimeConfig, /^default_agent:\s+xira-assistant$/m);
  assert.match(
    runtimeConfig,
    /^entrypoints:\s+\.\.\/demos\/websocket-agent\/entrypoints\.yaml$/m,
  );

  const channels = [...entrypointsConfig.matchAll(/^\s+channel:\s+(\S+)$/gm)].map(
    (match) => match[1],
  );
  assert.deepEqual(channels, ["websocket"]);
  assert.match(entrypointsConfig, /^\s+- id:\s+websocket-default$/m);
  assert.match(entrypointsConfig, /^\s+enabled:\s+true$/m);
  assert.match(entrypointsConfig, /^\s+default_agent:\s+xira-assistant$/m);
});
