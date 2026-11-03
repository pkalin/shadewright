# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

## Claude Code specifics

- `.claude/settings.json` asks before anything that touches a real cluster, Azure or GitHub (`kubectl apply|delete|exec`, `make deploy`, `git push`, `gh pr merge`). Keep it that way; local builds and tests are allowed.
- Changes to `api/`, RBAC markers, policy or the action catalog: run the `policy-reviewer` subagent (`.claude/agents/`) before calling the work done.
- Changes under `internal/`, `cmd/`, `api/` or `config/`: run the `docs-sync-reviewer` subagent before opening the pull request, and fix what it finds in the same branch. The pull request description says which docs changed, or why none needed to.
- Personal or machine-specific notes go in `CLAUDE.local.md` (git-ignored), never here.
