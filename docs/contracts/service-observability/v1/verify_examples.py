#!/usr/bin/env python3
"""Validate Service Observability v1 schemas, fixtures and RFC 6902 scenarios."""

from __future__ import annotations

import copy
import json
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry, Resource


BASE = Path(__file__).resolve().parent
EXAMPLES = BASE / "examples"


def load_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def decode_pointer(path: str) -> list[str]:
    if path == "":
        return []
    if not path.startswith("/"):
        raise ValueError(f"invalid JSON Pointer: {path}")
    return [part.replace("~1", "/").replace("~0", "~") for part in path[1:].split("/")]


def resolve_parent(document: Any, path: str) -> tuple[Any, str]:
    parts = decode_pointer(path)
    if not parts:
        raise ValueError("root replacement is not used by these fixtures")
    current = document
    for part in parts[:-1]:
        current = current[int(part)] if isinstance(current, list) else current[part]
    return current, parts[-1]


def pointer_get(document: Any, path: str) -> Any:
    current = document
    for part in decode_pointer(path):
        current = current[int(part)] if isinstance(current, list) else current[part]
    return current


def apply_json_patch(document: Any, operations: list[dict[str, Any]]) -> Any:
    result = copy.deepcopy(document)
    for operation in operations:
        op = operation["op"]
        path = operation["path"]
        if op not in {"add", "remove", "replace"}:
            raise ValueError(f"unsupported JSON Patch operation: {op}")
        parent, key = resolve_parent(result, path)
        if op == "remove":
            if isinstance(parent, list):
                del parent[int(key)]
            else:
                del parent[key]
            continue
        value = copy.deepcopy(operation["value"])
        if isinstance(parent, list):
            index = len(parent) if key == "-" else int(key)
            if op == "add":
                parent.insert(index, value)
            else:
                parent[index] = value
        else:
            if op == "replace" and key not in parent:
                raise KeyError(f"replace target does not exist: {path}")
            parent[key] = value
    return result


def ensure(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def unique(items: list[dict[str, Any]], field: str, label: str) -> set[str]:
    values = [item[field] for item in items]
    ensure(len(values) == len(set(values)), f"duplicate {label}: {values}")
    return set(values)


def build_registry() -> tuple[dict[str, Any], Registry]:
    schemas: dict[str, Any] = {}
    registry = Registry()
    for path in sorted(BASE.glob("*.schema.json")):
        schema = load_json(path)
        Draft202012Validator.check_schema(schema)
        schemas[path.name] = schema
        registry = registry.with_resource(schema["$id"], Resource.from_contents(schema))
    return schemas, registry


def validate_fixture(validator: Draft202012Validator, payload: Any, label: str) -> None:
    errors = sorted(validator.iter_errors(payload), key=lambda error: list(error.absolute_path))
    if not errors:
        print(f"PASS {label}")
        return
    messages = []
    for error in errors:
        location = "/".join(map(str, error.absolute_path)) or "<root>"
        messages.append(f"{location}: {error.message}")
    raise AssertionError(f"{label} failed schema validation:\n  " + "\n  ".join(messages))


def validate_semantic_references() -> None:
    profile = load_json(EXAMPLES / "e2b.profile.json")
    context = load_json(EXAMPLES / "e2b.context.json")
    directory = load_json(EXAMPLES / "e2b.directory-view.json")
    overview = load_json(EXAMPLES / "e2b.overview-view.json")
    workspace = load_json(EXAMPLES / "e2b.workspace-view.json")
    tools = load_json(EXAMPLES / "tool-catalog.json")

    tool_keys = unique(tools, "key", "tool key")
    focus_keys = unique(profile["focus_kinds"], "key", "focus key")
    journey_keys = unique(profile["journeys"], "key", "journey key")
    unique(profile["views"], "key", "view key")
    signal_keys = unique(profile["signal_bindings"], "key", "signal binding key")
    ensure(sum(1 for focus in profile["focus_kinds"] if focus["primary"]) == 1, "profile must have exactly one primary focus")
    ensure(set(profile["tool_refs"]) <= tool_keys, "profile references missing tools")
    for journey in profile["journeys"]:
        ensure(journey["focus_kind"] in focus_keys, f"unknown focus kind in journey {journey['key']}")
        unique(journey["stages"], "key", f"stage key in {journey['key']}")
    for view in profile["views"]:
        if "journey_ref" in view:
            ensure(view["journey_ref"] in journey_keys, f"unknown journey_ref in {view['key']}")
        ensure(set(view.get("signal_refs", [])) <= signal_keys, f"unknown signal_refs in {view['key']}")
        ensure(set(view.get("tool_refs", [])) <= tool_keys, f"unknown tool_refs in {view['key']}")

    ensure(context["service"]["service_id"] == profile["service_ref"]["service_id"], "context/profile service mismatch")
    ensure(context["service"]["profile_key"] == profile["profile_key"], "context/profile key mismatch")
    ensure(context["service"]["profile_version"] == profile["profile_version"], "context/profile version mismatch")
    context_signal_keys = unique(context["signals"], "binding_key", "context signal binding")
    ensure(context_signal_keys == signal_keys, f"context/profile signals differ: {context_signal_keys ^ signal_keys}")

    node_ids = unique(context["topology"]["nodes"], "entity_id", "context node id")
    relation_ids = unique(context["topology"]["relations"], "relation_id", "context relation id")
    for relation in context["topology"]["relations"]:
        ensure(relation["source_entity_id"] in node_ids and relation["target_entity_id"] in node_ids, f"dangling relation {relation['relation_id']}")
    for signal in context["signals"]:
        if signal["scope_kind"] == "service":
            ensure(signal["scope_ref"] == context["service"]["service_id"], f"signal {signal['binding_key']} has wrong service scope")
        elif signal["scope_kind"] in {"entity", "resource"}:
            ensure(signal["scope_ref"] in node_ids, f"signal {signal['binding_key']} has unknown node scope")
        elif signal["scope_kind"] == "relation":
            ensure(signal["scope_ref"] in relation_ids, f"signal {signal['binding_key']} has unknown relation scope")

    evidence_ids = unique(context["evidence_refs"], "evidence_id", "context evidence id")
    for evidence in context["evidence_refs"]:
        correlation = evidence["correlation"]
        ensure(set(correlation.get("entity_ids", [])) <= node_ids, f"evidence {evidence['evidence_id']} has dangling entity")
        ensure(set(correlation.get("relation_ids", [])) <= relation_ids, f"evidence {evidence['evidence_id']} has dangling relation")

    expected_golden = {signal["key"] for signal in profile["signal_bindings"] if signal["family"] == "golden"}
    for view in (directory["items"][0], overview, workspace):
        ensure(view["service"]["service_id"] == context["service"]["service_id"], "Page View service mismatch")
        ensure(view["profile"]["profile_key"] == profile["profile_key"], "Page View profile key mismatch")
        ensure(view["profile"]["profile_version"] == profile["profile_version"], "Page View profile version mismatch")
        ensure({signal["binding_key"] for signal in view["golden_signals"]} == expected_golden, "Page View Golden slots mismatch")

    workspace_node_ids = unique(workspace["topology"]["nodes"], "entity_id", "workspace node id")
    workspace_relation_ids = unique(workspace["topology"]["relations"], "relation_id", "workspace relation id")
    workspace_evidence_ids = unique(workspace["evidence_refs"], "evidence_id", "workspace evidence id")
    ensure(workspace_node_ids == node_ids, "workspace/context node ids differ")
    ensure(workspace_relation_ids == relation_ids, "workspace/context relation ids differ")
    ensure(workspace_evidence_ids == evidence_ids, "workspace/context evidence ids differ")

    journey = next(item for item in profile["journeys"] if item["key"] == workspace["focus"]["journey"])
    ensure([stage["key"] for stage in workspace["journey"]] == [stage["key"] for stage in journey["stages"]], "workspace/profile stage order mismatch")
    for stage_view, stage_definition in zip(workspace["journey"], journey["stages"]):
        ensure(set(stage_definition.get("entity_refs", [])) <= set(stage_view["entity_ids"]), f"stage {stage_view['key']} lost canonical entity refs")
        ensure(set(stage_definition.get("relation_refs", [])) <= set(stage_view["relation_ids"]), f"stage {stage_view['key']} lost canonical relation refs")
        ensure(set(stage_view["evidence_ids"]) <= workspace_evidence_ids, f"stage {stage_view['key']} has unknown evidence")
    ensure(set(workspace["diagnostic_summary"]["evidence_ids"]) <= workspace_evidence_ids, "diagnostic summary has unknown evidence")

    context_values = {
        signal["binding_key"]: (signal["value"], signal["availability"], signal["health"], signal.get("measurement_role"))
        for signal in context["signals"]
    }
    projected_signals = list(workspace["golden_signals"])
    for node in workspace["topology"]["nodes"]:
        projected_signals.extend(node["signals"])
    for relation in workspace["topology"]["relations"]:
        projected_signals.extend(relation["signals"])
    for signal in projected_signals:
        actual = (signal["value"], signal["availability"], signal["health"], signal.get("measurement_role"))
        ensure(actual == context_values[signal["binding_key"]], f"projection drift for {signal['binding_key']}")
    ensure({signal["binding_key"] for signal in projected_signals} == context_signal_keys, "not all context signals are projected")
    print("PASS semantic references and Context to Page View projection")


def validate_scenarios(page_validator: Draft202012Validator) -> None:
    manifest = load_json(EXAMPLES / "e2b.workspace-scenarios.json")
    ensure(manifest["schema_version"] == "1.0", "unsupported scenario fixture version")
    base_fixture = load_json(EXAMPLES / manifest["base_fixture"])
    unique(manifest["scenarios"], "key", "scenario key")
    for scenario in manifest["scenarios"]:
        result = apply_json_patch(base_fixture, scenario["patch"])
        validate_fixture(page_validator, result, f"scenario {scenario['key']} against Page View schema")
        for assertion in scenario["assertions"]:
            value = pointer_get(result, assertion["path"])
            if "equals" in assertion:
                ensure(value == assertion["equals"], f"{scenario['key']} assertion failed at {assertion['path']}: {value!r}")
            if "length" in assertion:
                ensure(len(value) == assertion["length"], f"{scenario['key']} length assertion failed at {assertion['path']}")
        available_evidence = {item["evidence_id"] for item in result["evidence_refs"]}
        ensure(set(result["diagnostic_summary"]["evidence_ids"]) <= available_evidence, f"{scenario['key']} diagnostic evidence leak")
        for stage in result["journey"]:
            ensure(set(stage["evidence_ids"]) <= available_evidence, f"{scenario['key']} stage evidence leak")


def validate_negative_invariants(
    page_validator: Draft202012Validator,
    context_validator: Draft202012Validator,
    evidence_validator: Draft202012Validator,
) -> None:
    workspace = load_json(EXAMPLES / "e2b.workspace-view.json")
    proxy_without_reason = copy.deepcopy(workspace)
    del proxy_without_reason["golden_signals"][3]["proxy_reason"]
    ensure(not page_validator.is_valid(proxy_without_reason), "proxy signal without proxy_reason was accepted")

    unavailable_with_value = copy.deepcopy(workspace)
    unavailable_with_value["topology"]["nodes"][2]["signals"][1]["value"] = 42
    ensure(not page_validator.is_valid(unavailable_with_value), "unavailable signal with numeric value was accepted")

    context_without_source = load_json(EXAMPLES / "e2b.context.json")
    del context_without_source["signals"][0]["source_key"]
    ensure(not context_validator.is_valid(context_without_source), "context signal without source_key was accepted")

    legacy_denied_evidence = copy.deepcopy(workspace["evidence_refs"][0])
    legacy_denied_evidence["availability"] = "denied"
    ensure(not evidence_validator.is_valid(legacy_denied_evidence), "legacy evidence availability=denied was accepted")
    print("PASS negative invariants for proxy labeling, unavailable values, source attribution and Forbidden vocabulary")


def main() -> None:
    schemas, registry = build_registry()
    format_checker = FormatChecker()
    fixtures = [
        ("service-observability-profile.schema.json", "e2b.profile.json", False),
        ("diagnostic-context.schema.json", "e2b.context.json", False),
        ("service-observability-page-view.schema.json", "e2b.directory-view.json", False),
        ("service-observability-page-view.schema.json", "e2b.overview-view.json", False),
        ("service-observability-page-view.schema.json", "e2b.workspace-view.json", False),
        ("tool-descriptor.schema.json", "tool-catalog.json", True),
    ]
    validators: dict[str, Draft202012Validator] = {}
    for schema_name, fixture_name, is_array in fixtures:
        validator = validators.setdefault(
            schema_name,
            Draft202012Validator(schemas[schema_name], registry=registry, format_checker=format_checker),
        )
        payload = load_json(EXAMPLES / fixture_name)
        items = payload if is_array else [payload]
        for index, item in enumerate(items):
            label = f"{fixture_name}[{index}] against {schema_name}" if is_array else f"{fixture_name} against {schema_name}"
            validate_fixture(validator, item, label)
    validate_semantic_references()
    page_validator = validators["service-observability-page-view.schema.json"]
    validate_scenarios(page_validator)
    context_validator = validators["diagnostic-context.schema.json"]
    evidence_validator = Draft202012Validator(
        schemas["evidence-ref.schema.json"], registry=registry, format_checker=format_checker
    )
    validate_negative_invariants(page_validator, context_validator, evidence_validator)
    print(f"PASS {len(schemas)} Draft 2020-12 schemas and all design fixtures")


if __name__ == "__main__":
    main()
