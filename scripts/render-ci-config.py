#!/usr/bin/env python3
# file: scripts/render-ci-config.py
# version: 1.0.2
"""Emit a burndown config.yaml from environment variables.

Replaces a fragile nested-bash-heredoc rendering step. All inputs come
from env so the workflow stays simple and auditable; the rendered file
is printed to stdout and (when --out is given) written to disk.

Required env:
  MODE              - dry-run | draft-only | full
  WORKSPACE         - GitHub Actions workspace root (for repo local_path)
  RUNNER_TEMP       - GitHub Actions runner temp dir (for state/digest paths)

Optional env (all-or-none):
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

    app_id = os.environ.get("GH_APP_ID", "").strip()
    install_id = os.environ.get("GH_APP_INSTALLATION_ID", "").strip()
    pem_path = os.environ.get("GH_APP_PEM_PATH", "").strip()
    have_app = bool(app_id and install_id and pem_path)

    sections = [
        f"""\
openai:
  api_key_env: OPENAI_API_KEY

triage:
  provider: openai
  # gpt-5-mini is plenty for triage classification + branch-name-suggestion;
  # cheaper and lower-token-per-call than full gpt-5 so it doesn't compete
  # for TPM with the implementer.
  model: gpt-5-mini

implementer:
  provider: openai
  # codex-mini variant: optimized for code generation, lower per-call token
  # cost than full gpt-5, and on a separate TPM bucket from the
  # general-purpose chat models. Lets the matrix run more cells in parallel
  # without saturating any single bucket.
  model: gpt-5.1-codex-mini

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
"""
    ]

    if have_app:
        sections.append(
            f"""\
github:
  app_id: {app_id}
  installation_id: {install_id}
  private_key_path: {pem_path}
"""
        )

    sections.append(
        f"""\
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
"""
    )

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
