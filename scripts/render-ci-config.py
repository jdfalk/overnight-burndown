#!/usr/bin/env python3
# file: scripts/render-ci-config.py
# version: 1.1.0
"""Emit a burndown config.yaml from environment variables.

Replaces a fragile nested-bash-heredoc rendering step. All inputs come
from env so the workflow stays simple and auditable; the rendered file
is printed to stdout and (when --out is given) written to disk.

Required env:
  MODE              - dry-run | draft-only | full
  WORKSPACE         - GitHub Actions workspace root (for repo local_path)
  RUNNER_TEMP       - GitHub Actions runner temp dir (for state/digest paths)

Optional env:
  IMPLEMENTER_PROVIDER  - openai | anthropic  (default: openai)
                          Controls which LLM backend runs the agent loop.
                          Set via the workflow_dispatch 'implementer_provider'
                          input; the scheduled nightly always uses 'openai'.

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
    provider = os.environ.get("IMPLEMENTER_PROVIDER", "openai").strip().lower()

    app_id = os.environ.get("GH_APP_ID", "").strip()
    install_id = os.environ.get("GH_APP_INSTALLATION_ID", "").strip()
    pem_path = os.environ.get("GH_APP_PEM_PATH", "").strip()
    have_app = bool(app_id and install_id and pem_path)

    sections: list[str] = []

    # ------------------------------------------------------------------
    # Provider credentials — only emit the block(s) needed by the run.
    # ------------------------------------------------------------------
    if provider == "openai":
        sections.append("""\
openai:
  api_key_env: OPENAI_API_KEY
""")
    elif provider == "anthropic":
        sections.append("""\
anthropic:
  api_key_env: ANTHROPIC_API_KEY
""")

    # ------------------------------------------------------------------
    # Triage — always on OpenAI (gpt-5-mini is cheap + fast for metadata
    # classification).  When the whole run is on Anthropic, triage could
    # move to Claude; flip to provider: anthropic + model: claude-haiku-*
    # once Claude API credits are available.
    # ------------------------------------------------------------------
    sections.append("""\
triage:
  provider: openai
  model: gpt-5-mini
""")

    # ------------------------------------------------------------------
    # Implementer — selected via IMPLEMENTER_PROVIDER.
    # ------------------------------------------------------------------
    if provider == "anthropic":
        sections.append("""\
# Anthropic implementer — uses Claude haiku/sonnet/opus via model_tiers.
# Activate by setting IMPLEMENTER_PROVIDER=anthropic in the workflow
# dispatch input, or by running:
#   MODE=dry-run IMPLEMENTER_PROVIDER=anthropic ... render-ci-config.py
implementer:
  provider: anthropic
  model: claude-haiku-4-5-20251001   # catch-all when no tier matches
  model_tiers:
    - model: claude-haiku-4-5-20251001   # simplest tasks — complexity 1–2
      max_complexity: 2
    - model: claude-sonnet-4-6           # medium — complexity 3–4
      max_complexity: 4
    - model: claude-opus-4-7             # hardest — complexity 5
    # No max_complexity on the last tier = catch-all for any score above 4.
    # No runtime fallback chain: Anthropic has no PreviousResponseID
    # equivalent, so mid-task model swap would re-upload full history.
    # Tier selection (pick-once-before-loop) is the right model here.
""")
    else:
        # Default: OpenAI Responses path with tier selection + fallback chain.
        sections.append("""\
# OpenAI implementer — uses Responses API (/v1/responses) with
# PreviousResponseID threading. Tier selection picks the cheapest model
# expected to handle the task's complexity; the fallback chain escalates
# when that model exhausts its 429-retry budget. PreviousResponseID
# carries the conversation across the swap with no token re-upload.
implementer:
  provider: openai
  model: gpt-5.1-codex-mini    # catch-all when no tier matches
  model_tiers:
    - model: gpt-5.1-codex-mini    # simple tasks — complexity 1–2
      max_complexity: 2
    - model: gpt-5.3-codex         # moderate — complexity 3–4
      max_complexity: 4
    - model: gpt-5                 # hardest — complexity 5
    # No max_complexity on the last entry = catch-all for complexity 5+.
    # FallbacksFrom(complexity) automatically builds the runtime chain from
    # tiers above the selected one, so escalation is config-free.
  # api: responses   (default — set to chat-completions to revert to the
  # legacy /v1/chat/completions path during soak)
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
  max_dollars: 5.0
  max_wall_seconds: 3000
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

    sections.append(f"""\
repos:
  - name: audiobook-organizer
    owner: jdfalk
    local_path: {workspace}/targets/audiobook-organizer
    mode: {mode}
    # Exclude heavy fixtures from per-task worktrees so 15 parallel worktrees
    # fit on a stock GitHub Actions runner (14 GB ephemeral disk). Without
    # this, ~1.4 GB of LibriVox audio per worktree ENOSPCs around task 5.
    worktree_exclude_paths:
      - testdata/audio
""")

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
