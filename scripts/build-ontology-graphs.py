#!/usr/bin/env python3
"""Generate deterministic routing graphs. Requires scripts/ontology-requirements.txt."""
import argparse
import json
from pathlib import Path
import sys

import yaml


def build(root):
    ontology = root / ".claude/ontology"
    agents = yaml.safe_load((ontology / "agents.yaml").read_text())["agents"]
    skills = yaml.safe_load((ontology / "skills.yaml").read_text())["skills"]
    rules = yaml.safe_load((ontology / "rules.yaml").read_text())["rules"]
    for name in agents:
        if not (root / ".claude/agents" / (name + ".md")).is_file():
            raise ValueError(f"missing agent source: {name}")
    for name in skills:
        if not (root / ".claude/skills" / name / "SKILL.md").is_file():
            raise ValueError(f"missing skill source: {name}")
    for name, spec in rules.items():
        if not (root / ".claude/rules" / spec["filename"]).is_file():
            raise ValueError(f"missing rule source: {name}")

    def mapping(source, field, targets):
        edges = {name: sorted(set(spec.get(field, []))) for name, spec in sorted(source.items())}
        reverse = {name: [] for name in sorted(targets)}
        for name, refs in edges.items():
            for ref in refs:
                if ref not in targets:
                    raise ValueError(f"{name}.{field}: unknown reference {ref}")
                reverse[ref].append(name)
        return edges, reverse

    agent_edges, skill_reverse = mapping(agents, "skills", skills)
    skill_edges, rule_reverse = mapping(skills, "rule_references", rules)
    routing_skills = {name: spec for name, spec in skills.items() if "routes_to" in spec}
    routing_edges, router_reverse = mapping(routing_skills, "routes_to", agents)
    agent_to_router = {}
    for agent, routers in router_reverse.items():
        if len(routers) > 1:
            raise ValueError(f"ambiguous routing for {agent}: {routers}")
        if routers:
            agent_to_router[agent] = routers[0]

    nodes = {}
    for kind, items in [("Agent", agents), ("Skill", skills), ("Rule", rules)]:
        for name, spec in sorted(items.items()):
            if name in nodes:
                raise ValueError(f"duplicate node: {name}")
            nodes[name] = {"type": kind, "class": spec["class"]}
    edges = []
    adjacency = {name: {} for name in nodes}
    for mapping_edges, relation, inverse in [
        (agent_edges, "requires", "required_by"),
        (skill_edges, "depends_on", "referenced_by"),
        (routing_edges, "routes_to", "routed_by"),
    ]:
        for source, targets in mapping_edges.items():
            adjacency[source][relation] = targets
            for target in targets:
                edges.append({"source": source, "target": target, "relation": relation})
                adjacency[target].setdefault(inverse, []).append(source)

    def graph(description, **data):
        return {"description": description, "version": "1.0.0", **data}

    return {
        "agent-skill.json": graph("Agent to Skill dependency mapping", edges=agent_edges, reverse=skill_reverse),
        "skill-rule.json": graph("Skill to Rule dependency mapping", edges=skill_edges, reverse=rule_reverse),
        "routing.json": graph("Routing skill to Agent mapping", routes={
            name: {"agents": refs, "description": skills[name]["description"]}
            for name, refs in routing_edges.items()
        }, agent_to_router=agent_to_router),
        "full-graph.json": graph("Complete ontology graph for traversal", nodes=nodes, edges=edges, adjacency=adjacency),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail if generated graphs are stale")
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    args = parser.parse_args()
    stale = []
    for name, graph in build(args.root).items():
        path = args.root / ".claude/ontology/graphs" / name
        content = json.dumps(graph, ensure_ascii=False, indent=2) + "\n"
        if args.check:
            if not path.exists() or path.read_text() != content:
                stale.append(str(path.relative_to(args.root)))
        else:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content)
    if stale:
        print("Stale ontology graphs: " + ", ".join(stale), file=sys.stderr)
        print("Run: python3 scripts/build-ontology-graphs.py", file=sys.stderr)
        return 1
    print("Ontology graphs are current" if args.check else "Generated 4 ontology graphs")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, yaml.YAMLError) as exc:
        sys.exit(f"Ontology validation failed: {exc}")
