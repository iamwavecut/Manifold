#!/usr/bin/env python3
"""Pure unit tests; no GitHub request, SSH connection, or production mutation."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('release_request', Path(__file__).with_name('geta-release-request.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseRequestTests(unittest.TestCase):
    def fixture(self, root):
        for name in release.PROFILE_FILES:
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('fixture\n')
        (root / 'DATA_VERSION').write_text('4\n')
        return {
            'DEPLOY_OPERATION': 'deploy',
            'DEPLOY_SHA': 'a' * 40,
            'MANIFOLD_IMAGE_DIGEST': 'sha256:' + 'b' * 64,
            'BRAIN_IMAGE_DIGEST': 'sha256:' + 'c' * 64,
        }

    def test_immutable_release_and_profile_change(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = self.fixture(root)
            before = release.release_payload(environment, root, lambda: 'synthetic-token')
            self.assertEqual(before['data_version'], '4')
            self.assertEqual(set(before['images']), {'brain', 'manifold'})
            self.assertNotIn('project', before)
            self.assertNotIn('operation', before)
            (root / release.PROFILE_FILES[1]).write_text('changed\n')
            after = release.release_payload(environment, root, lambda: 'synthetic-token')
            self.assertNotEqual(before['profile_digest'], after['profile_digest'])

    def test_invalid_digest_refused_before_token_request(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = self.fixture(root)
            environment['BRAIN_IMAGE_DIGEST'] = 'latest'
            with self.assertRaises(ValueError):
                release.release_payload(environment, root, lambda: self.fail('token requested before validation'))

    def test_rollback_contains_only_identity(self):
        payload = release.release_payload({'DEPLOY_OPERATION': 'rollback'}, Path('/unused'), lambda: 'synthetic-token')
        self.assertEqual(payload, {'token': 'synthetic-token'})

    def test_workflow_has_no_arbitrary_upload_or_host_script(self):
        workflow = Path(__file__).resolve().parents[1] / '.github/workflows/deploy-production.yml'
        source = workflow.read_text()
        self.assertIn('id-token: write', source)
        self.assertIn('"$DEPLOY_USER" == deploy', source)
        self.assertNotIn('scp ', source)
        self.assertNotIn('/tmp/manifold-deploy-', source)
        self.assertIn('MANIFOLD_IMAGE_DIGEST:', source)
        self.assertIn('BRAIN_IMAGE_DIGEST:', source)


if __name__ == '__main__':
    unittest.main()
