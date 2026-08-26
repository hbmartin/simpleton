import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

test("rejects a mismatched protocol", async () => {
  const child = spawn(process.execPath, [new URL("./index.mjs", import.meta.url).pathname], { stdio: ["pipe", "pipe", "inherit"] });
  child.stdin.end(`${JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "999" } })}\n`);
  let output = "";
  for await (const chunk of child.stdout) output += chunk;
  const response = JSON.parse(output);
  assert.equal(response.error.code, -32002);
});

async function runPack(requests) {
  const child = spawn(process.execPath, [new URL("./index.mjs", import.meta.url).pathname], { stdio: ["pipe", "pipe", "inherit"] });
  child.stdin.end(`${requests.map((request) => JSON.stringify(request)).join("\n")}\n`);
  let output = "";
  for await (const chunk of child.stdout) output += chunk;
  return output.trim().split("\n").map((line) => JSON.parse(line));
}

async function repository(t, files) {
  const repo = await mkdtemp(path.join(tmpdir(), "simpleton-typescript-pack-"));
  t.after(() => rm(repo, { recursive: true, force: true }));
  execFileSync("git", ["init", "-q", repo]);
  execFileSync("git", ["-C", repo, "config", "user.email", "simpleton@example.invalid"]);
  execFileSync("git", ["-C", repo, "config", "user.name", "Simpleton Test"]);
  for (const [name, content] of Object.entries(files)) await writeFile(path.join(repo, name), content, "utf8");
  execFileSync("git", ["-C", repo, "add", "."]);
  execFileSync("git", ["-C", repo, "commit", "-qm", "sample"]);
  const revision = execFileSync("git", ["-C", repo, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  return { repo, revision };
}

test("uses qualified 64-bit target identities", async (t) => {
  const { repo, revision } = await repository(t, {
    "sample.ts": "class NamedFirst { same() { return 1; } }\nclass NamedSecond { same() { return 2; } }\nconst First = class { same() { return 3; } };\nconst Second = class { same() { return 4; } };\n",
    "caller.ts": "export function invoke(value: { same(): number }) { return value.same(); }\n",
  });
  const responses = await runPack([
    { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1" } },
    { jsonrpc: "2.0", id: 2, method: "analyze", params: {
      repository: repo, head_revision: revision, changed_files: [{ path: "sample.ts", language: "typescript" }], budget_ms: 1000,
    } },
  ]);
  const targets = responses[1].result.targets;
  assert.deepEqual(new Set(targets.map((target) => target.symbol)), new Set(["NamedFirst.same", "NamedSecond.same", "First.same", "Second.same"]));
  assert.equal(new Set(targets.map((target) => target.id)).size, 4);
  assert.ok(targets.every((target) => target.id.length === 16));
  assert.ok(targets.every((target) => target.observation_candidates.every((boundary) => boundary.kind !== "unchanged_caller")));
});

test("restores caller scope after nested functions", async (t) => {
  const { repo, revision } = await repository(t, {
    "target.ts": "export function target() { return 1; }\n",
    "caller.ts": "import { target } from './target';\nfunction outer() { function inner() { target(); } target(); }\ntarget();\n",
  });
  const responses = await runPack([
    { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1" } },
    { jsonrpc: "2.0", id: 2, method: "analyze", params: {
      repository: repo, head_revision: revision, changed_files: [{ path: "target.ts", language: "typescript" }], budget_ms: 1000,
    } },
  ]);
  const target = responses[1].result.targets.find((candidate) => candidate.symbol === "target");
  const callers = target.observation_candidates.filter((boundary) => boundary.kind === "unchanged_caller").map((boundary) => boundary.symbol);
  assert.deepEqual(new Set(callers), new Set(["outer.inner", "outer", "<module>"]));
});
