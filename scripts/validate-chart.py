#!/usr/bin/env python3
"""Validate the Falcon chart by rendering it and parsing the result.

Helm concatenates template outputs without understanding YAML, so defects
like a missing "---" between conditional blocks surface only when the joined
manifest is parsed. This script renders the chart (with default values and
with every component enabled), then checks each document: valid YAML without
duplicate keys, required fields present, and no two resources sharing an
identity.

Usage: validate-chart.py [chart-path]
"""

import subprocess
import sys
import tempfile
from pathlib import Path

import yaml

CHART = Path(__file__).resolve().parent.parent / "charts" / "falcon"

# Overlay that turns on every conditional template block.
ALL_ON = """
webui: {enabled: true}
admin: {enabled: true, host: admin.example.org}
zfsAgent: {enabled: true}
"""


class DuplicateKey(Exception):
    pass


def construct_mapping(loader, node, deep=False):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in mapping:
            raise DuplicateKey(
                f"duplicate key {key!r} at line {key_node.start_mark.line + 1}"
            )
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping


class StrictLoader(yaml.SafeLoader):
    pass


StrictLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping
)


def render(chart: Path, values: str | None) -> list[dict]:
    cmd = ["helm", "template", "falcon", str(chart), "-n", "default"]
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as f:
        if values:
            f.write(values)
            f.flush()
            cmd += ["-f", f.name]
        out = subprocess.run(cmd, check=True, capture_output=True, text=True).stdout
    docs = []
    for i, chunk in enumerate(out.split("\n---\n")):
        try:
            doc = yaml.load(chunk, Loader=StrictLoader)
        except DuplicateKey as e:
            sys.exit(f"FAIL: document {i}: {e}")
        except yaml.YAMLError as e:
            sys.exit(f"FAIL: document {i}: invalid YAML: {e}")
        if doc:
            docs.append(doc)
    return docs


def check(docs: list[dict]) -> list[str]:
    problems, seen = [], set()
    for d in docs:
        for key in ("apiVersion", "kind", "metadata"):
            if key not in d:
                problems.append(f"document missing {key!r}: {list(d)[:4]}")
        meta = d.get("metadata", {})
        ident = (d.get("kind"), meta.get("name"), meta.get("namespace"))
        if ident in seen:
            problems.append(f"duplicate resource identity {ident}")
        seen.add(ident)
    return problems


def main() -> None:
    chart = Path(sys.argv[1]) if len(sys.argv) > 1 else CHART
    renders = {"defaults": render(chart, None), "all-on": render(chart, ALL_ON)}

    failures = []
    for label, docs in renders.items():
        failures += [f"[{label}] {p}" for p in check(docs)]
        print(f"[{label}] {len(docs)} resources rendered")

    identities = {
        (d.get("kind"), d.get("metadata", {}).get("name"))
        for d in renders["all-on"]
    }
    failures += [
        f"expected resource absent: {r}"
        for r in [("ClusterRole", "falcon-pv-reader"), ("Deployment", "falcon")]
        if r not in identities
    ]

    if failures:
        sys.exit("\n".join(f"FAIL: {f}" for f in failures))
    print("OK: chart renders into distinct, well-formed documents")


if __name__ == "__main__":
    main()
