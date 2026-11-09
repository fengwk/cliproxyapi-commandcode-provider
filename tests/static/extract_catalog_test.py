#!/usr/bin/env python3
"""Static tests for scripts/extract-cli-catalog.py.

Synthetic mini-bundles stand in for the command-code CLI so the extractor is
exercised offline without the real 3.4 MB dist/cli.mjs: object-boundary
strictness, the supported fact literals, fail-closed behaviour on ambiguous or
unsupported input, and the extract/check/diff commands.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCRIPT = os.path.join(ROOT, "scripts", "extract-cli-catalog.py")
VERSION = "9.9.9"
DATE = "2026-11-09"
ENTRY_COUNT = 44  # comfortably above the extractor's 40-entry floor


def entry(index, extra=""):
    return 'M_{0}:{{id:"model-{0}",inputModalities:["text"],name:"Model {0}"{1}}}'.format(index, extra)


def bundle(entries, version=VERSION, tail=""):
    """Wrap entries in an assignment plus a decoy template/regex outside the map."""
    junk = 'const banner=`hello ${name} /not-a-regex/ ${"nested"}`;'
    return junk + 'var x="' + version + '",RL={' + ",".join(entries) + "};" + tail


def base_entries(count=ENTRY_COUNT, start=0):
    return [entry(i) for i in range(start, start + count)]


class ToolCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def path(self, name):
        return os.path.join(self.tmp.name, name)

    def write(self, name, text):
        path = self.path(name)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(text)
        return path

    def run_tool(self, *args):
        return subprocess.run(
            [sys.executable, SCRIPT, *args],
            capture_output=True,
            text=True,
            check=False,
        )

    def extract(self, source, *args):
        bundle_path = self.write("cli.mjs", source)
        out = self.path("cli.json")
        done = self.run_tool("extract", "--bundle", bundle_path, "--version", VERSION, "--date", DATE, "--out", out, *args)
        self.assertEqual(done.returncode, 0, done.stderr)
        with open(out, encoding="utf-8") as handle:
            return json.load(handle)

    # --- 1. every fact stays inside its own object boundary ------------------
    def test_facts_never_cross_object_boundaries(self):
        entries = [entry(0, ',contextWindow:105e4,maxOutputTokens:32768,reasoning:!0')] + base_entries(ENTRY_COUNT - 1, 1)
        models = self.extract(bundle(entries))["models"]
        first, second = models[0], models[1]
        self.assertEqual(first["contextWindow"], 1050000)
        self.assertEqual(first["maxOutputTokens"], 32768)
        self.assertIs(first["reasoning"], True)
        # The following entry declares none of these, so none may leak into it.
        for field in ("contextWindow", "maxOutputTokens", "reasoning", "reasoningEfforts"):
            self.assertNotIn(field, second, field + " leaked across entries")
        self.assertEqual(second["id"], "model-1")
        self.assertEqual(len(models), ENTRY_COUNT)

    def test_identifier_values_and_getters_are_not_recorded(self):
        entries = [entry(0, ',provider:QF,spec:nN,get hidden(){return!isLive("x")}')] + base_entries(ENTRY_COUNT - 1, 1)
        models = self.extract(bundle(entries))["models"]
        self.assertEqual(models[0], {"id": "model-0", "name": "Model 0", "inputModalities": ["text"]})
        self.assertNotIn("provider", models[0])

    # --- 2. the supported fact literals -------------------------------------
    def test_supported_fact_literals(self):
        hot = entry(
            0,
            ',contextWindow:1e6,maxOutputTokens:32768,reasoning:!1,'
            'reasoningEfforts:["low","medium",\'high\']',
        )
        models = self.extract(bundle([hot] + base_entries(ENTRY_COUNT - 1, 1)))["models"]
        self.assertEqual(
            models[0],
            {
                "id": "model-0",
                "name": "Model 0",
                "inputModalities": ["text"],
                "reasoningEfforts": ["low", "medium", "high"],
                "contextWindow": 1000000,
                "maxOutputTokens": 32768,
                "reasoning": False,
            },
        )

    # --- 3. ambiguous or unsupported input fails closed ----------------------
    def test_ambiguous_model_maps_fail(self):
        # Two candidate maps each pass the signature, so the tool must refuse
        # instead of picking one.
        source = bundle(base_entries()) + "var RL2={" + ",".join(base_entries()) + "};"
        done = self.run_tool("extract", "--bundle", self.write("cli.mjs", source), "--version", VERSION)
        self.assertEqual(done.returncode, 2)
        self.assertIn("ambiguous", done.stderr)

    def test_unsupported_literals_fail(self):
        for extra in (',name2:`tpl ${x}`', ',meta:{nested:1}', ',note:"a"+"b"', ',tail:[{x:1}]'):
            with self.subTest(extra=extra):
                source = bundle([entry(0, extra)] + base_entries(ENTRY_COUNT - 1, 1))
                done = self.run_tool("extract", "--bundle", self.write("cli.mjs", source), "--version", VERSION)
                self.assertEqual(done.returncode, 2, done.stdout)
                self.assertIn("error:", done.stderr)

    def test_partial_catalog_is_refused(self):
        done = self.run_tool("extract", "--bundle", self.write("cli.mjs", bundle(base_entries(3))), "--version", VERSION)
        self.assertEqual(done.returncode, 2)
        self.assertIn("too few entries", done.stderr)

    # --- 4. extract / check / diff commands ---------------------------------
    def test_extract_headers_and_diff(self):
        first = bundle(base_entries(ENTRY_COUNT))
        second = bundle(
            [entry(0).replace('"Model 0"', '"Renamed 0"')]
            + base_entries(ENTRY_COUNT - 2, 2)
            + [entry(ENTRY_COUNT)]
        )
        old_path = self.write("old.json", json.dumps(self.extract(first)))
        snapshot = self.extract(second, "--diff", old_path)
        self.assertEqual(snapshot["source"], "Installed command-code CLI static catalog, facts only")
        self.assertEqual(snapshot["cli_version"], VERSION)
        self.assertEqual(snapshot["date"], DATE)
        self.assertEqual(len(snapshot["models"]), ENTRY_COUNT)
        done = self.run_tool("extract", "--bundle", self.write("cli2.mjs", second), "--version", VERSION,
                             "--date", DATE, "--diff", old_path)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn("added 1: model-" + str(ENTRY_COUNT), done.stdout)
        self.assertIn("removed 1: model-1", done.stdout)
        self.assertIn("Model 0' -> 'Renamed 0", done.stdout)

    def test_check_accepts_matching_and_rejects_drifted_snapshot(self):
        source = bundle(base_entries())
        bundle_path = self.write("cli.mjs", source)
        snapshot = self.extract(source)
        snapshot_path = self.write("cli.json", json.dumps(snapshot, indent=2) + "\n")
        ok = self.run_tool("check", "--bundle", bundle_path, "--snapshot", snapshot_path)
        self.assertEqual(ok.returncode, 0, ok.stderr)
        self.assertIn("{0} models".format(ENTRY_COUNT), ok.stdout)

        snapshot["models"][0]["contextWindow"] = 1
        drifted = self.write("drifted.json", json.dumps(snapshot, indent=2) + "\n")
        bad = self.run_tool("check", "--bundle", bundle_path, "--snapshot", drifted)
        self.assertEqual(bad.returncode, 1)
        self.assertIn("MISMATCH", bad.stderr)
        self.assertIn("contextWindow", bad.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
