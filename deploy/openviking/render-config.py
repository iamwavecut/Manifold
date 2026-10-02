"""Render OpenViking's config with deployment request controls, then start it.

OpenViking's storage binding reads the config file before environment
placeholders are expanded, so the template stays valid JSON and keeps its
${...} placeholders. Only vlm.extra_request_body, which must be a JSON object
rather than a string, is filled in here from OPENVIKING_VLM_EXTRA_BODY.
"""

import json
import os
import sys

template, target = sys.argv[1], os.environ["OPENVIKING_CONFIG_FILE"]
with open(template, encoding="utf-8") as source:
    config = json.load(source)

extra = os.environ.get("OPENVIKING_VLM_EXTRA_BODY", "").strip()
if extra:
    body = json.loads(extra)
    if not isinstance(body, dict):
        raise SystemExit("OPENVIKING_VLM_EXTRA_BODY must be a JSON object")
    config["vlm"]["extra_request_body"] = body

with open(target, "w", encoding="utf-8") as rendered:
    json.dump(config, rendered, indent=2)

os.execvp("openviking-entrypoint", ["openviking-entrypoint", *sys.argv[2:]])
