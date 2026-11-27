#!/usr/bin/env python3
"""Tests for scripts/brain-flags.py."""

import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("brain-flags.py")


def run(baseline, current, adopt="", out=None, check=False):
    with tempfile.TemporaryDirectory() as tmp:
        root = pathlib.Path(tmp)
        (root / "baseline.json").write_text(json.dumps(baseline))
        (root / "current.json").write_text(json.dumps(current))
        (root / "adopt.txt").write_text(adopt)
        target = root / "defaults.env"
        if out is not None:
            target.write_text(out)
        args = [
            sys.executable, str(SCRIPT),
            "--baseline", str(root / "baseline.json"),
            "--current", str(root / "current.json"),
            "--adopt", str(root / "adopt.txt"),
            "--out", str(target),
        ]
        if check:
            args.append("--check")
        result = subprocess.run(args, capture_output=True, text=True)
        written = target.read_text() if target.exists() else None
        return result, written


def values(text):
    return [line for line in text.splitlines() if line and not line.startswith("#")]


def flag(default, boolean=False):
    return {"default": default, "boolean": boolean}


class BrainFlags(unittest.TestCase):
    def assert_values(self, baseline, current, expected, adopt=""):
        result, written = run(baseline, current, adopt)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(values(written), expected)

    def test_a_switched_on_default_is_pinned_off(self):
        self.assert_values({"A": flag("0", True)}, {"A": flag("1", True)}, ["A=0"])

    def test_a_changed_number_keeps_the_old_value(self):
        self.assert_values({"T": flag("60000")}, {"T": flag("20000")}, ["T=60000"])

    def test_a_new_on_flag_is_pinned_off(self):
        self.assert_values({}, {"B": flag("1", True), "C": flag("true", True)}, ["B=0", "C=0"])

    def test_unchanged_removed_new_off_and_new_value_keys_write_nothing(self):
        self.assert_values(
            {"SAME": flag("1", True), "GONE": flag("1", True)},
            {"SAME": flag("1", True), "OFF": flag("0", True), "NEW_VALUE": flag("5")},
            [],
        )

    def test_an_adopted_key_writes_nothing(self):
        self.assert_values(
            {"A": flag("0", True)},
            {"A": flag("1", True), "B": flag("1", True)},
            [],
            adopt="# reason\nA\nB  # why\n",
        )

    def test_a_changed_key_without_a_baseline_default_fails(self):
        result, written = run({"A": flag(None)}, {"A": flag("1")})
        self.assertEqual(result.returncode, 1)
        self.assertIn("A", result.stderr)
        self.assertIsNone(written)

    def test_a_value_with_a_space_or_hash_is_quoted(self):
        self.assert_values({"S": flag("a #b")}, {"S": flag("x")}, ['S="a #b"'])

    def test_check_accepts_an_up_to_date_file(self):
        _, written = run({"A": flag("0", True)}, {"A": flag("1", True)})
        result, _ = run({"A": flag("0", True)}, {"A": flag("1", True)}, out=written, check=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_check_exits_1_when_the_file_differs(self):
        result, written = run(
            {"A": flag("0", True)}, {"A": flag("1", True)}, out="A=1\n", check=True
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(written, "A=1\n")


if __name__ == "__main__":
    unittest.main()
