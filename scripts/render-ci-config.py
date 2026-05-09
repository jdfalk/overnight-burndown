#!/usr/bin/env python3
# file: scripts/render-ci-config.py
# version: 1.4.0
"""Emit a burndown config.yaml from environment variables.

Replaces a fragile nested-bash-heredoc rendering step. All inputs come
from env so the workflow stays simple and auditable; the rendered file
is printed to stdout and (when --out is given) written to disk.

Required env:
  MODE              - dry-run | draft-only | full
  WORKSPACE         - GitHub Actions workspace root (for repo local_path)
  RUNNER_TEMP       - GitHub Actions runner temp dir (for state/digest paths)

Optional env:
  REPO_NAME         - target repo name (default: audiobook-organizer)
  REPO_OWNER        - target repo owner (default: jdfalk)

  IMPLEMENTER_PROVIDER  - openai | anthropic  (default: openai)
                          Controls which LLM backend runs the agent loop.

  TRIAGE_MODEL      - OpenAI model for triage (default: o4-mini)
                      Override when a different model is preferred for task
                      classification; any model accessible to OPENAI_API_KEY.

  CHEAPEST_ONLY     - 1 | true | yes  (default: false)
                      When set, emits only the cheapest single model for the
                      provider with no model_tiers escalation chain.

  POWERFUL_ONLY     - 1 | true | yes  (default: false)
                      When set, emits only the most powerful single model for
                      the provider. Mutually exclusive with CHEAPEST_ONLY;
                      POWERFUL_ONLY wins.

  GH_APP_ID
  GH_APP_INSTALLATION_ID
  GH_APP_PEM_PATH   - path to materialized PEM file
"""

from __future__ import annotations

import argparse
import os
import sys


def render() -> str:
    mode = os.environ["MODE"]
    workspace = os.environ["WORKSPACE"]
    tmp = os.environ["RUNNER_TEMP"]
    repo_name = os.environ.get("REPO_NAME", "audiobook-organizer").strip()
    repo_owner = os.environ.get("REPO_OWNER", "jdfalk").strip()
    provider = os.environ.get("IMPLEMENTER_PROVIDER", "openai").strip().lower()
    triage_model = os.environ.get("TRIAGE_MODEL", "o4-mini").strip()
    cheapest_only = os.environ.get("CHEAPEST_ONLY", "").strip().lower() in ("1", "true", "yes")
    powerful_only = os.environ.get("POWERFUL_ONLY", "").strip().lower() in ("1", "true", "yes")
    if powerful_only:
        cheapest_only = False  # powerful wins

    app_id = os.environ.get("GH_APP_ID", "").strip()
    install_id = os.environ.get("GH_APP_INSTALLATION_ID", "").strip()
    pem_path = os.environ.get("GH_APP_PEM_PATH", "").strip()
    have_app = bool(app_id and install_id and pem_path)

    sections: list[str] = []

    # ------------------------------------------------------------------
    # Provider credentials. Include whichever providers are needed.
    # ------------------------------------------------------------------
    sections.append("""\
openai:
  api_key_env: OPENAI_API_KEY
""")
    if provider == "anthropic":
        sections.append("""\
anthropic:
  api_key_env: ANTHROPIC_API_KEY
""")

    # ------------------------------------------------------------------
    # Triage — always OpenAI. o4-mini has native reasoning which gives
    # Opus-class cross-cutting constraint analysis at a fraction of the
    # cost. Override with TRIAGE_MODEL env var if a different model is
    # preferred (e.g. gpt-5 for maximum quality).
    # ------------------------------------------------------------------
    sections.append(f"""\
triage:
  provider: openai
  model: {triage_model}
""")

    # ------------------------------------------------------------------
    # Implementer — selected via IMPLEMENTER_PROVIDER.
    # ------------------------------------------------------------------
    if provider == "anthropic":
        if cheapest_only:
            sections.append("""\
# Anthropic implementer — cheapest-only mode (no tier escalation).
implementer:
  provider: anthropic
  model: claude-haiku-4-5-20251001
""")
        elif powerful_only:
            sections.append("""\
# Anthropic implementer — powerful-only mode (opus for all tasks).
# Use when cheaper models return no-change on real multi-file work.
implementer:
  provider: anthropic
  model: claude-opus-4-7
""")
        else:
            sections.append("""\
# Anthropic implementer — haiku/sonnet/opus tiers by complexity.
implementer:
  provider: anthropic
  model: claude-haiku-4-5-20251001   # catch-all when no tier matches
  model_tiers:
    - model: claude-haiku-4-5-20251001   # simplest tasks — complexity 1–2
      max_complexity: 2
    - model: claude-sonnet-4-6           # medium — complexity 3–4
      max_complexity: 4
    - model: claude-opus-4-7             # hardest — complexity 5
""")
    else:
        # Default: OpenAI Responses path.
        if cheapest_only:
            sections.append("""\
# OpenAI implementer — cheapest-only mode (codex-mini, no tier escalation).
implementer:
  provider: openai
  model: gpt-5.1-codex-mini
""")
        elif powerful_only:
            sections.append("""\
# OpenAI implementer — powerful-only mode (gpt-5 for all tasks).
# Use when cheaper models return no-change on real multi-file work.
# gpt-5 has higher TPM limits so fewer 429s at this concurrency level.
implementer:
  provider: openai
  model: gpt-5
""")
        else:
            sections.append("""\
# OpenAI implementer — Responses API with tier escalation.
# Tier selection picks the cheapest model for the task complexity;
# fallback chain escalates on 429-budget exhaustion.
implementer:
  provider: openai
  model: gpt-5.1-codex-mini    # catch-all when no tier matches
  model_tiers:
    - model: gpt-5.1-codex-mini    # simple tasks — complexity 1–2
      max_complexity: 2
    - model: gpt-5.3-codex         # moderate — complexity 3–4
      max_complexity: 4
    - model: gpt-5                 # hardest — complexity 5
""")

    # ------------------------------------------------------------------
    # Shared infrastructure
    # ------------------------------------------------------------------
    sections.append(f"""\
paths:
  state_dir: {tmp}/burndown-state
  worktree_root: {tmp}/burndown-state/worktrees
  digest_dir: {tmp}/burndown-digest
  audit_dir: {tmp}/burndown-state/audit
  log_dir: {tmp}/burndown-state/logs

budget:
  max_dollars: {"20.0" if powerful_only else "5.0"}
  max_wall_seconds: {"5400" if powerful_only else "3000"}
  abort_threshold: 0.8

concurrency:
  max_parallel_agents: 4

defaults:
  mode: {mode}
  ci_watch_timeout_seconds: 1800
  diff_size_cap_lines: 200
  task_priority: cheap-first
  auto_merge_paths: ["*.md"]
  forced_review_paths: [".github/workflows/**"]
""")

    if have_app:
        sections.append(f"""\
github:
  app_id: {app_id}
  installation_id: {install_id}
  private_key_path: {pem_path}
""")

    repo_section = f"""\
repos:
  - name: {repo_name}
    owner: {repo_owner}
    local_path: {workspace}/targets/{repo_name}
    mode: {mode}
"""
    if repo_name == "audiobook-organizer":
        repo_section += """\
    # Exclude heavy fixtures from per-task worktrees so 15 parallel worktrees
    # fit on a stock GitHub Actions runner (14 GB ephemeral disk). Without
    # this, ~1.4 GB of LibriVox audio per worktree ENOSPCs around task 5.
    worktree_exclude_paths:
      - testdata/audio
"""
    sections.append(repo_section)

    return "\n".join(sections)


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--out", help="Write rendered config here. If omitted, prints to stdout only.")
    args = p.parse_args()

    cfg = render()
    sys.stdout.write(cfg)
    if args.out:
        with open(args.out, "w") as f:
            f.write(cfg)
    return 0


if __name__ == "__main__":
    sys.exit(main())
