#!/usr/bin/env python3
"""Exercise deployment boundaries with isolated Git repos and fake runtime tools."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("deploy-production.sh")


class DeploymentBoundaryTests(unittest.TestCase):
    def run_deployment(self, old_version, new_version):
        with tempfile.TemporaryDirectory(prefix="manifold-deploy-test-") as directory:
            root = Path(directory)
            origin = root / "origin"
            checkout = root / "checkout"
            binaries = root / "bin"
            origin.mkdir()
            binaries.mkdir()

            def git(*args, cwd=origin):
                return subprocess.check_output(
                    ["git", *args], cwd=cwd, text=True, stderr=subprocess.DEVNULL
                ).strip()

            git("init", "-b", "main")
            git("config", "user.name", "Deployment test")
            git("config", "user.email", "test@example.invalid")
            (origin / ".gitignore").write_text(".env\n")
            (origin / "VERSION").write_text("1.0.0\n")
            skill = origin / "skills/manifold/scripts/manifold"
            skill.parent.mkdir(parents=True)
            skill.write_text('''#!/bin/sh
case "$*" in
  *status*) echo '{"state": "ready"}' ;;
  *context*) echo '{"context": {}}' ;;
  *) echo '{"items": []}' ;;
esac
''')
            if old_version is not None:
                (origin / "DATA_VERSION").write_text(old_version + "\n")
            git("add", ".")
            git("commit", "-m", "Baseline fixture")
            previous_sha = git("rev-parse", "HEAD")
            (origin / "DATA_VERSION").write_text(new_version + "\n")
            (origin / "change.txt").write_text("Fixture change\n")
            git("add", ".")
            git("commit", "-m", "Target fixture")
            target_sha = git("rev-parse", "HEAD")
            git("clone", str(origin), str(checkout))
            git("reset", "--hard", previous_sha, cwd=checkout)
            (checkout / ".env").write_text(
                "MANIFOLD_PUBLIC_URL=http://example.invalid\n"
                "MANIFOLD_BOOTSTRAP_API_KEY=synthetic-test-key\n"
            )
            (checkout / ".env").chmod(0o600)
            runtime_log = root / "runtime.log"
            (binaries / "docker").write_text('''#!/bin/sh
printf '%s\n' "$*" >> "$TEST_RUNTIME_LOG"
case "$*" in
  *"manifold version"*) echo "1.0.0 $TEST_TARGET_SHA" ;;
esac
''')
            (binaries / "curl").write_text('''#!/bin/sh
case "$*" in
  *api/v1/meta*) printf '{"version":"1.0.0","commit":"%s","protocol_revision":2}\n' "$TEST_TARGET_SHA" ;;
  *) echo '{}' ;;
esac
''')
            for executable in binaries.iterdir():
                executable.chmod(0o755)
            lock = root / "deploy.lock"
            environment = os.environ | {
                "PATH": str(binaries) + os.pathsep + os.environ["PATH"],
                "MANIFOLD_REPO_DIR": str(checkout),
                "MANIFOLD_DEPLOY_LOCK": str(lock),
                "TEST_RUNTIME_LOG": str(runtime_log),
                "TEST_TARGET_SHA": target_sha,
            }
            result = subprocess.run(
                ["bash", str(SCRIPT), target_sha],
                env=environment, text=True, capture_output=True, timeout=20,
            )
            return (
                result,
                git("rev-parse", "HEAD", cwd=checkout),
                previous_sha,
                target_sha,
                runtime_log.read_text() if runtime_log.exists() else "",
                lock.exists(),
            )

    def test_migration_boundary_refuses_before_checkout_or_runtime_changes(self):
        result, head, previous, _, runtime, locked = self.run_deployment(None, "2")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("data compatibility boundary", result.stderr)
        self.assertEqual(head, previous)
        self.assertNotIn(" up ", runtime)
        self.assertNotIn(" stop ", runtime)
        self.assertFalse(locked)

    def test_same_data_version_keeps_automatic_deployment(self):
        result, head, _, target, runtime, locked = self.run_deployment("2", "2")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(head, target)
        self.assertIn(" up ", runtime)
        self.assertFalse(locked)


if __name__ == "__main__":
    unittest.main()
