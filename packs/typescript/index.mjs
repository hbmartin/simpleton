#!/usr/bin/env node
import { execFileSync } from "node:child_process";
import readline from "node:readline";
import path from "node:path";
import ts from "typescript";

const PROTOCOL_VERSION = "1";
const PACK_VERSION = "0.1.0";
const MAX_GIT_OUTPUT = 32 * 1024 * 1024;

const capability = {
  language: "typescript",
  pack_version: PACK_VERSION,
  protocol_version: PROTOCOL_VERSION,
  methods: { analyze: true, probe: false, cancel: false, types: true, callers: true, effects: true },
  requirements: { runtime: "node>=20", compiler: `typescript@${ts.version}` },
  trust_classes: ["trusted_branch"],
};

const input = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
for await (const line of input) {
  if (!line.trim()) continue;
  let request;
  try {
    request = JSON.parse(line);
    const result = dispatch(request.method, request.params ?? {});
    process.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id: request.id, result })}\n`);
  } catch (error) {
    process.stdout.write(`${JSON.stringify({
      jsonrpc: "2.0",
      id: request?.id ?? null,
      error: { code: -32002, message: error instanceof Error ? error.message : String(error) },
    })}\n`);
  }
}

function dispatch(method, params) {
  if (method === "initialize") {
    if (params.protocol_version !== PROTOCOL_VERSION) throw new Error("protocol version mismatch");
    return { capability };
  }
  if (method === "analyze") return analyze(params);
  if (method === "probe") {
    return {
      method: { id: "typescript_native_probe", language: "typescript", status: "unsupported", reason: "native probe generation is not implemented" },
      divergences: [], replay_capsules: [],
    };
  }
  if (method === "cancel") return { cancelled: true };
  throw new Error(`method not found: ${method}`);
}

function analyze(params) {
  const started = Date.now();
  const repo = path.resolve(params.repository);
  const changed = new Set((params.changed_files ?? []).filter((file) => file.language === "typescript").map((file) => file.path));
  const paths = listFiles(repo, params.head_revision).filter((file) => /\.(?:[cm]?ts|tsx)$/.test(file) && !file.endsWith(".d.ts"));
  const sourceByAbsolute = new Map();
  for (const file of paths) {
    try {
      sourceByAbsolute.set(path.join(repo, file), showFile(repo, params.head_revision, file));
    } catch {
      // A generated or submodule-owned file may be unavailable to git show.
    }
  }
  const options = { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, allowJs: false, skipLibCheck: true, jsx: ts.JsxEmit.Preserve };
  const host = ts.createCompilerHost(options, true);
  const originalFileExists = host.fileExists.bind(host);
  const originalReadFile = host.readFile.bind(host);
  host.fileExists = (fileName) => sourceByAbsolute.has(path.resolve(fileName)) || originalFileExists(fileName);
  host.readFile = (fileName) => sourceByAbsolute.get(path.resolve(fileName)) ?? originalReadFile(fileName);
  host.getSourceFile = (fileName, languageVersion) => {
    const text = host.readFile(fileName);
    return text === undefined ? undefined : ts.createSourceFile(fileName, text, languageVersion, true);
  };
  const program = ts.createProgram([...sourceByAbsolute.keys()], options, host);
  const checker = program.getTypeChecker();
  const typeDiagnostics = ts.getPreEmitDiagnostics(program).filter((diagnostic) =>
    diagnostic.category === ts.DiagnosticCategory.Error && diagnostic.file && !slash(path.relative(repo, diagnostic.file.fileName)).startsWith("..")
  );
  const callers = collectCallers(program, repo, changed);
  const targets = [];
  const opportunities = [];
  for (const source of program.getSourceFiles()) {
    const relative = slash(path.relative(repo, source.fileName));
    if (!changed.has(relative)) continue;
    visitFunctions(source, (node, name) => {
      const risks = effectRisks(node, source);
      const boundaries = (callers.get(name) ?? []).map((caller) => ({
        kind: "unchanged_caller", symbol: caller.symbol, path: caller.path, stable: true, generated: false, confidence: 0.82,
      }));
      if (isExported(node)) boundaries.push({ kind: "public_api", symbol: name, path: relative, stable: true, generated: false, confidence: 0.75 });
      boundaries.push({ kind: "direct_unit", symbol: name, path: relative, stable: false, generated: false, confidence: 0.5 });
      const id = stableID("typescript", relative, name);
      targets.push({
        id, language: "typescript", file: relative, symbol: name, kind: "function", scope_type: "symbol",
        observation_candidates: boundaries,
        applicability: {
          applicable: risks.length === 0,
          reason: risks.length === 0 ? "no statically detected effects" : "effect risks require an approved stable observation boundary",
          confidence: clamp(0.88 - risks.length * 0.12), risks,
        },
      });
      const statements = countStatements(node);
      const complexity = cyclomatic(node);
      if (statements >= 30 || complexity >= 10) {
        opportunities.push({
          id: stableID("typescript-opportunity", relative, name), language: "typescript", category: "oversized_or_complex_unit",
          region: `${relative}:${name}`, evidence: [`statements=${statements}`, `cyclomatic=${complexity}`], allowed_files: [relative],
          benefit: clamp(statements / 80 + complexity / 30), applicability_confidence: clamp(0.88 - risks.length * 0.12),
          risk: clamp(risks.length / 5), review_effort: clamp(statements / 100), rank: 0,
        });
      }
      if (node.name) checker.getTypeAtLocation(node.name);
    });
  }
  return {
    targets,
    methods: [{
      id: "typescript_native_analysis", language: "typescript", status: "ran", duration_ms: Date.now() - started,
      budget: `${params.budget_ms ?? 0}ms`, findings: [],
      coverage: { targets_total: targets.length, targets_observed: targets.length, ratio: targets.length ? 1 : 0 },
    }, {
      id: "typescript_type_analysis", language: "typescript", status: typeDiagnostics.length ? "inconclusive" : "ran",
      reason: typeDiagnostics.length ? `${typeDiagnostics.length} compiler diagnostics; first: ${ts.flattenDiagnosticMessageText(typeDiagnostics[0].messageText, " ")}` : "",
      findings: [],
    }],
    opportunities,
  };
}

function listFiles(repo, revision) {
  return execFileSync("git", ["-C", repo, "ls-tree", "-r", "--name-only", "-z", revision], {
    encoding: "utf8",
    maxBuffer: MAX_GIT_OUTPUT,
  }).split("\0").filter(Boolean);
}

function showFile(repo, revision, file) {
  return execFileSync("git", ["-C", repo, "show", `${revision}:${file}`], { encoding: "utf8", maxBuffer: MAX_GIT_OUTPUT });
}

function collectCallers(program, repo, changed) {
  const callers = new Map();
  for (const source of program.getSourceFiles()) {
    const relative = slash(path.relative(repo, source.fileName));
    if (changed.has(relative) || relative.startsWith("..")) continue;
    let enclosing = "<module>";
    const visit = (node) => {
      if (isFunctionLike(node)) enclosing = functionName(node) || enclosing;
      if (ts.isCallExpression(node)) {
        const name = calledName(node.expression);
        if (name) {
          const entries = callers.get(name) ?? [];
          entries.push({ path: relative, symbol: enclosing });
          callers.set(name, entries);
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
  }
  return callers;
}

function visitFunctions(source, callback) {
  const visit = (node) => {
    if (isFunctionLike(node)) {
      const name = functionName(node);
      if (name) callback(node, name);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
}

function isFunctionLike(node) {
  return ts.isFunctionDeclaration(node) || ts.isMethodDeclaration(node) || ts.isFunctionExpression(node) || ts.isArrowFunction(node);
}

function functionName(node) {
  if (node.name && ts.isIdentifier(node.name)) return node.name.text;
  if (node.parent && ts.isVariableDeclaration(node.parent) && ts.isIdentifier(node.parent.name)) return node.parent.name.text;
  return "";
}

function calledName(expression) {
  if (ts.isIdentifier(expression)) return expression.text;
  if (ts.isPropertyAccessExpression(expression)) return expression.name.text;
  return "";
}

function isExported(node) {
  return Boolean(node.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword)) ||
    (node.parent && ts.isSourceFile(node.parent) && functionName(node).startsWith("use"));
}

function effectRisks(node, source) {
  const risks = new Set();
  for (const statement of source.statements) {
    if (ts.isImportDeclaration(statement)) {
      const module = statement.moduleSpecifier.text;
      if (/^(?:node:)?(?:fs|net|http|https|crypto|worker_threads|child_process|timers)/.test(module)) risks.add(`imports_${module.replace(/\W/g, "_")}`);
    }
  }
  if (node.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.AsyncKeyword)) risks.add("async");
  const visit = (child) => {
    if (ts.isAwaitExpression(child)) risks.add("async");
    if (ts.isNewExpression(child) && calledName(child.expression) === "Date") risks.add("time");
    if (ts.isCallExpression(child)) {
      const name = calledName(child.expression);
      if (["fetch", "setTimeout", "setInterval", "random", "now", "writeFile", "readFile"].includes(name)) risks.add(`call_${name}`);
    }
    ts.forEachChild(child, visit);
  };
  visit(node);
  return [...risks].sort();
}

function countStatements(node) {
  let count = 0;
  const visit = (child) => {
    if (ts.isStatement(child)) count += 1;
    ts.forEachChild(child, visit);
  };
  visit(node.body ?? node);
  return count;
}

function cyclomatic(node) {
  let complexity = 1;
  const visit = (child) => {
    if (ts.isIfStatement(child) || ts.isForStatement(child) || ts.isForInStatement(child) || ts.isForOfStatement(child) ||
        ts.isWhileStatement(child) || ts.isDoStatement(child) || ts.isCaseClause(child) || ts.isConditionalExpression(child) ||
        (ts.isBinaryExpression(child) && [ts.SyntaxKind.AmpersandAmpersandToken, ts.SyntaxKind.BarBarToken, ts.SyntaxKind.QuestionQuestionToken].includes(child.operatorToken.kind))) {
      complexity += 1;
    }
    ts.forEachChild(child, visit);
  };
  visit(node.body ?? node);
  return complexity;
}

function stableID(...parts) {
  let hash = 2166136261;
  for (const character of parts.join("\0")) {
    hash ^= character.codePointAt(0);
    hash = Math.imul(hash, 16777619);
  }
  return (hash >>> 0).toString(16).padStart(8, "0");
}

function clamp(value) { return Math.max(0, Math.min(1, value)); }
function slash(value) { return value.split(path.sep).join("/"); }
