#!/usr/bin/env python3
"""Regression checks for ontology regeneration and stale detection."""
import importlib.util
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("build-ontology-graphs.py")
ROOT = SCRIPT.resolve().parents[1]
spec = importlib.util.spec_from_file_location("ontology", SCRIPT)
ontology = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ontology)


class GraphTests(unittest.TestCase):
    def test_current_source_nodes_and_references(self):
        graph = ontology.build(ROOT)["full-graph.json"]
        self.assertIn("lang-java-expert", graph["nodes"])
        self.assertNotIn("lang-java21-expert", graph["nodes"])
        for edge in graph["edges"]:
            self.assertIn(edge["source"], graph["nodes"])
            self.assertIn(edge["target"], graph["nodes"])
            self.assertIn(edge["target"], graph["adjacency"][edge["source"]][edge["relation"]])

    def test_stale_check_and_missing_agent_fail(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for directory in ("ontology", "agents", "skills", "rules"):
                shutil.copytree(ROOT / ".claude" / directory, root / ".claude" / directory)
            command = [sys.executable, str(SCRIPT), "--root", str(root)]
            subprocess.run(command, check=True, capture_output=True)
            subprocess.run(command + ["--check"], check=True, capture_output=True)
            (root / ".claude/ontology/graphs/routing.json").write_text("{}")
            result = subprocess.run(command + ["--check"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("Stale ontology graphs", result.stderr)
            (root / ".claude/agents/lang-java-expert.md").unlink()
            result = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("missing agent source", result.stderr)


if __name__ == "__main__":
    unittest.main()
