import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import test from "node:test";

test("rejects a mismatched protocol", async () => {
  const child = spawn(process.execPath, [new URL("./index.mjs", import.meta.url).pathname], { stdio: ["pipe", "pipe", "inherit"] });
  child.stdin.end(`${JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "999" } })}\n`);
  let output = "";
  for await (const chunk of child.stdout) output += chunk;
  const response = JSON.parse(output);
  assert.equal(response.error.code, -32002);
});
