#!/usr/bin/env python3
"""Emit a bounded release request on stdout; never log the OIDC bearer token."""
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import urllib.parse
import urllib.request

PROFILE_FILES = (
    'docker-compose.yml',
    'deploy/openviking/ov.conf',
    'deploy/openviking/render-config.py',
)


def release_payload(environment, root, get_token):
    operation = environment.get('DEPLOY_OPERATION', 'deploy')
    if operation not in ('deploy', 'rollback'):
        raise ValueError('unsupported deployment operation')
    if operation == 'rollback':
        return {'token': get_token()}
    if not re.fullmatch(r'[0-9a-f]{40}', environment.get('DEPLOY_SHA', '')):
        raise ValueError('a full target commit is required')
    images = {
        'manifold': environment.get('MANIFOLD_IMAGE_DIGEST', ''),
        'brain': environment.get('BRAIN_IMAGE_DIGEST', ''),
    }
    if any(not re.fullmatch(r'sha256:[0-9a-f]{64}', value) for value in images.values()):
        raise ValueError('both immutable image digests are required')
    digest = hashlib.sha256()
    for name in PROFILE_FILES:
        digest.update(name.encode() + b'\0' + (root / name).read_bytes() + b'\0')
    data_version = (root / 'DATA_VERSION').read_text().strip()
    if not re.fullmatch(r'[0-9]+', data_version):
        raise ValueError('invalid data compatibility version')
    return {
        'token': get_token(),
        'images': images,
        'data_version': data_version,
        'profile_digest': digest.hexdigest(),
    }


def github_identity():
    address = os.environ['ACTIONS_ID_TOKEN_REQUEST_URL']
    if urllib.parse.urlsplit(address).scheme != 'https':
        raise ValueError('GitHub identity endpoint must use HTTPS')
    separator = '&' if '?' in address else '?'
    request = urllib.request.Request(
        address + separator + urllib.parse.urlencode({'audience': 'geta-deploy'}),
        headers={'Authorization': 'Bearer ' + os.environ['ACTIONS_ID_TOKEN_REQUEST_TOKEN']},
    )
    with urllib.request.urlopen(request, timeout=15) as response:
        result = json.load(response)
    token = result.get('value')
    if not isinstance(token, str) or not token or len(token) > 16000:
        raise ValueError('GitHub returned an invalid identity')
    return token


def main():
    try:
        payload = release_payload(os.environ, Path(__file__).resolve().parents[1], github_identity)
    except Exception:
        # OIDC responses and exceptions must not accidentally disclose credentials.
        print('could not prepare authenticated deployment request', file=sys.stderr)
        return 1
    json.dump(payload, sys.stdout)
    sys.stdout.write('\n')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
