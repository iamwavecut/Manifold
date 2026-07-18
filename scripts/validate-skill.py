#!/usr/bin/env python3
"""Minimal portable validation for the repository's external agent skill."""

import re
import sys
from pathlib import Path


root = Path(sys.argv[1] if len(sys.argv) > 1 else "skills/manifold")
skill = root / "SKILL.md"
readme = Path("README.md")
errors = []
if not skill.is_file():
    errors.append(f"missing {skill}")
else:
    text = skill.read_text(encoding="utf-8")
    frontmatter = re.match(r"^---\n(.*?)\n---\n", text, re.DOTALL)
    if not frontmatter:
        errors.append("SKILL.md must start with YAML frontmatter")
    else:
        header = frontmatter.group(1)
        if not re.search(r"^name:\s*manifold\s*$", header, re.MULTILINE):
            errors.append("frontmatter name must be manifold")
        description = re.search(r"^description:\s*(.+)$", header, re.MULTILINE)
        if not description or len(description.group(1).strip()) < 80:
            errors.append("frontmatter description must explain concrete triggers")
    if "TODO" in text:
        errors.append("SKILL.md still contains TODO")
    for relative in [
        "agents/openai.yaml",
        "scripts/manifold",
        "references/error-codes.md",
        "bin/darwin-amd64/manifold",
        "bin/darwin-arm64/manifold",
        "bin/linux-amd64/manifold",
        "bin/linux-arm64/manifold",
    ]:
        if not (root / relative).is_file():
            errors.append(f"missing {relative}")

if not readme.is_file():
    errors.append("missing README.md")
else:
    readme_text = readme.read_text(encoding="utf-8")
    if "npx skills add iamwavecut/Manifold" not in readme_text:
        errors.append("README.md must install the skill with npx skills")
    if 'cp -R skills/manifold' in readme_text or '.codex/skills/manifold' in readme_text:
        errors.append("README.md must not document manual agent-directory installation")

if errors:
    for error in errors:
        print("ERROR:", error, file=sys.stderr)
    raise SystemExit(1)
print("Skill is valid.")
