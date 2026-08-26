#!/usr/bin/env python3
from __future__ import annotations

import json
import pathlib
import sys

import jsonschema
import yaml
from referencing import Registry, Resource


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCHEMAS = ROOT / "schemas" / "v1"


def load(name: str) -> dict:
    return json.loads((SCHEMAS / name).read_text())


def main() -> int:
    for path in sorted(SCHEMAS.glob("*.json")):
        jsonschema.Draft202012Validator.check_schema(json.loads(path.read_text()))

    policy_schema = load("policy.schema.json")
    jsonschema.validate(yaml.safe_load((ROOT / ".simpleton" / "policy.yaml").read_text()), policy_schema)

    intent_schema = load("change-intent.schema.json")
    observation_schema = load("observation-spec.schema.json")
    registry = Registry().with_resource(
        observation_schema["$id"], Resource.from_contents(observation_schema)
    )
    valid_intent = {
        "schema_version": "1",
        "id": "schema-fixture",
        "rationale": "Exercise public schema validation",
        "classification": "behavior_preserving",
        "scope": [{"path": "calc.go"}],
        "allowed_changes": [],
        "equivalence_contract": {
            "api_mappings": [],
            "observations": [{
                "id": "add",
                "target": {"language": "go", "scope_type": "symbol", "path": "calc.go", "symbol": "Add"},
                "inputs": [[1, 2]],
                "preconditions": [{"built_in": "finite"}],
                "calls_or_events": ["Add"],
                "observables": ["return"],
                "comparator": {"built_in": "exact"},
            }],
        },
    }
    validator = jsonschema.Draft202012Validator(intent_schema, registry=registry)
    validator.validate(valid_intent)

    invalid_inline = json.loads(json.dumps(valid_intent))
    invalid_inline["equivalence_contract"]["observations"][0]["preconditions"][0] = {"script": "rm -rf /"}
    try:
        validator.validate(invalid_inline)
    except jsonschema.ValidationError:
        pass
    else:
        raise AssertionError("inline executable YAML shape was accepted")

    if len(sys.argv) == 3 and sys.argv[1] == "--evidence":
        jsonschema.validate(json.loads(pathlib.Path(sys.argv[2]).read_text()), load("evidence-pack.schema.json"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
