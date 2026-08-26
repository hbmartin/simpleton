#!/usr/bin/env python3
from __future__ import annotations

import ast
import hashlib
import json
import pathlib
import subprocess
import sys
import time
from collections import defaultdict
from typing import Any

PROTOCOL_VERSION = "1"
PACK_VERSION = "0.1.1"

CAPABILITY = {
    "language": "python",
    "pack_version": PACK_VERSION,
    "protocol_version": PROTOCOL_VERSION,
    "methods": {"analyze": True, "probe": False, "cancel": False, "types": False, "type_hints": True, "imports": True, "callers": True, "effects": True},
    "requirements": {"runtime": f"python>={sys.version_info.major}.{sys.version_info.minor}"},
    "trust_classes": ["trusted_branch"],
}


def main() -> int:
    for line in sys.stdin:
        if not line.strip():
            continue
        request: dict[str, Any] | None = None
        try:
            request = json.loads(line)
            result = dispatch(request.get("method", ""), request.get("params") or {})
            response = {"jsonrpc": "2.0", "id": request.get("id"), "result": result}
        except Exception as error:  # the RPC boundary must return structured failure
            response = {
                "jsonrpc": "2.0",
                "id": request.get("id") if request else None,
                "error": {"code": -32002, "message": str(error)},
            }
        sys.stdout.write(json.dumps(response, separators=(",", ":")) + "\n")
        sys.stdout.flush()
    return 0


def dispatch(method: str, params: dict[str, Any]) -> dict[str, Any]:
    if method == "initialize":
        if params.get("protocol_version") != PROTOCOL_VERSION:
            raise ValueError("protocol version mismatch")
        return {"capability": CAPABILITY}
    if method == "analyze":
        return analyze(params)
    if method == "probe":
        return {
            "method": {
                "id": "python_native_probe",
                "language": "python",
                "status": "unsupported",
                "reason": "native probe generation is not implemented",
            },
            "divergences": [],
            "replay_capsules": [],
        }
    if method == "cancel":
        return {"cancelled": True}
    raise ValueError(f"method not found: {method}")


def analyze(params: dict[str, Any]) -> dict[str, Any]:
    started = time.monotonic()
    repo = pathlib.Path(params["repository"]).resolve()
    revision = params["head_revision"]
    changed = {
        item["path"]
        for item in params.get("changed_files", [])
        if item.get("language") == "python"
    }
    trees: dict[str, ast.Module] = {}
    for file in list_files(repo, revision):
        if not file.endswith((".py", ".pyi")):
            continue
        try:
            source = show_file(repo, revision, file)
            tree = ast.parse(source, filename=file, type_comments=True)
            trees[file] = tree
        except (OSError, UnicodeError, SyntaxError, subprocess.CalledProcessError):
            continue
    targets_by_name: defaultdict[str, list[str]] = defaultdict(list)
    for file in sorted(changed):
        tree = trees.get(file)
        if tree is None:
            continue
        for node, symbol in qualified_functions(tree):
            targets_by_name[node.name].append(symbol)
    unambiguous_targets = {
        name: symbols[0] for name, symbols in targets_by_name.items() if len(symbols) == 1
    }
    callers = collect_callers(trees, changed, unambiguous_targets)
    targets: list[dict[str, Any]] = []
    opportunities: list[dict[str, Any]] = []
    for file in sorted(changed):
        tree = trees.get(file)
        if tree is None:
            continue
        for node, symbol in qualified_functions(tree):
            risks = effect_risks(node, tree)
            boundaries = [
                {
                    "kind": "unchanged_caller",
                    "symbol": caller["symbol"],
                    "path": caller["path"],
                    "stable": True,
                    "generated": False,
                    "confidence": 0.78,
                }
                for caller in callers.get(symbol, [])
            ]
            if not node.name.startswith("_"):
                boundaries.append(
                    {
                        "kind": "public_api",
                        "symbol": symbol,
                        "path": file,
                        "stable": True,
                        "generated": False,
                        "confidence": 0.68,
                    }
                )
            boundaries.append(
                {
                    "kind": "direct_unit",
                    "symbol": symbol,
                    "path": file,
                    "stable": False,
                    "generated": False,
                    "confidence": 0.45,
                }
            )
            confidence = clamp(0.82 - len(risks) * 0.12)
            targets.append(
                {
                    "id": stable_id("python", file, symbol),
                    "language": "python",
                    "file": file,
                    "symbol": symbol,
                    "kind": "function",
                    "scope_type": "symbol",
                    "observation_candidates": boundaries,
                    "applicability": {
                        "applicable": not risks,
                        "reason": "no statically detected effects"
                        if not risks
                        else "dynamic or effect risks require an approved stable observation boundary",
                        "confidence": confidence,
                        "risks": risks,
                    },
                }
            )
            statements = sum(isinstance(child, ast.stmt) for child in ast.walk(node))
            complexity = cyclomatic(node)
            if statements >= 30 or complexity >= 10:
                opportunities.append(
                    {
                        "id": stable_id("python-opportunity", file, symbol),
                        "language": "python",
                        "category": "oversized_or_complex_unit",
                        "region": f"{file}:{symbol}",
                        "evidence": [f"statements={statements}", f"cyclomatic={complexity}"],
                        "allowed_files": [file],
                        "benefit": clamp(statements / 80 + complexity / 30),
                        "applicability_confidence": confidence,
                        "risk": clamp(len(risks) / 5),
                        "review_effort": clamp(statements / 100),
                        "rank": 0,
                    }
                )
    elapsed = int((time.monotonic() - started) * 1000)
    return {
        "targets": targets,
        "methods": [
            {
                "id": "python_native_analysis",
                "language": "python",
                "status": "ran",
                "budget": f"{params.get('budget_ms', 0)}ms",
                "duration_ms": elapsed,
                "findings": [],
                "coverage": {
                    "targets_total": len(targets),
                    "targets_observed": len(targets),
                    "ratio": 1 if targets else 0,
                },
            }
        ],
        "opportunities": opportunities,
    }


def qualified_functions(
    tree: ast.Module,
) -> list[tuple[ast.FunctionDef | ast.AsyncFunctionDef, str]]:
    functions: list[tuple[ast.FunctionDef | ast.AsyncFunctionDef, str]] = []

    class Visitor(ast.NodeVisitor):
        def __init__(self) -> None:
            self.scope: list[str] = []

        def visit_ClassDef(self, node: ast.ClassDef) -> None:
            self.scope.append(node.name)
            self.generic_visit(node)
            self.scope.pop()

        def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
            symbol = ".".join([*self.scope, node.name])
            functions.append((node, symbol))
            self.scope.append(node.name)
            self.generic_visit(node)
            self.scope.pop()

        def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
            symbol = ".".join([*self.scope, node.name])
            functions.append((node, symbol))
            self.scope.append(node.name)
            self.generic_visit(node)
            self.scope.pop()

    Visitor().visit(tree)
    return functions


def collect_callers(
    trees: dict[str, ast.Module],
    changed: set[str],
    unambiguous_targets: dict[str, str],
) -> dict[str, list[dict[str, str]]]:
    callers: defaultdict[str, list[dict[str, str]]] = defaultdict(list)
    for file, tree in trees.items():
        if file in changed:
            continue
        for function, caller_symbol in qualified_functions(tree):
            class CallVisitor(ast.NodeVisitor):
                def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
                    if node is function:
                        self.generic_visit(node)

                def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
                    if node is function:
                        self.generic_visit(node)

                def visit_Call(self, node: ast.Call) -> None:
                    name = called_name(node.func)
                    target = unambiguous_targets.get(name)
                    if target:
                        callers[target].append({"path": file, "symbol": caller_symbol})
                    self.generic_visit(node)

            CallVisitor().visit(function)
    return callers


def called_name(node: ast.expr) -> str:
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return node.attr
    return ""


def effect_risks(node: ast.FunctionDef | ast.AsyncFunctionDef, tree: ast.Module) -> list[str]:
    risks: set[str] = set()
    imports: set[str] = set()
    for child in tree.body:
        if isinstance(child, ast.Import):
            imports.update(alias.name.split(".")[0] for alias in child.names)
        elif isinstance(child, ast.ImportFrom) and child.module:
            imports.add(child.module.split(".")[0])
    for module in {"os", "io", "time", "random", "secrets", "socket", "http", "asyncio", "threading", "subprocess"} & imports:
        risks.add(f"imports_{module}")
    if isinstance(node, ast.AsyncFunctionDef):
        risks.add("async")
    for child in ast.walk(node):
        if isinstance(child, (ast.Await, ast.Yield, ast.YieldFrom)):
            risks.add("async_or_generator")
        elif isinstance(child, (ast.Global, ast.Nonlocal)):
            risks.add("external_state_write")
        elif isinstance(child, ast.Call):
            name = called_name(child.func)
            if name in {"open", "print", "sleep", "time", "random", "uuid4", "request", "send", "recv"}:
                risks.add(f"call_{name}")
    return sorted(risks)


def cyclomatic(node: ast.AST) -> int:
    branches = (
        ast.If,
        ast.For,
        ast.AsyncFor,
        ast.While,
        ast.IfExp,
        ast.ExceptHandler,
        ast.With,
        ast.AsyncWith,
        ast.Match,
        ast.comprehension,
    )
    complexity = 1
    for child in ast.walk(node):
        if isinstance(child, branches):
            complexity += 1
        elif isinstance(child, ast.BoolOp):
            complexity += max(1, len(child.values) - 1)
    return complexity


def list_files(repo: pathlib.Path, revision: str) -> list[str]:
    output = subprocess.check_output(
        ["git", "-C", str(repo), "ls-tree", "-r", "--name-only", "-z", revision]
    )
    return [entry.decode() for entry in output.split(b"\0") if entry]


def show_file(repo: pathlib.Path, revision: str, file: str) -> str:
    return subprocess.check_output(
        ["git", "-C", str(repo), "show", f"{revision}:{file}"]
    ).decode()


def stable_id(*parts: str) -> str:
    return hashlib.sha256("\0".join(parts).encode()).hexdigest()[:16]


def clamp(value: float) -> float:
    return max(0.0, min(1.0, value))


if __name__ == "__main__":
    raise SystemExit(main())
