# Contributing

Shadewright is early and small. The most useful contributions right now are a reproducible bug, an incident
the operator diagnoses or refuses wrongly, a scenario for the lab, and tasks from the issue list.

## Where things are

- Issues for all three repositories ([shadewright](https://github.com/pkalin/shadewright),
  [shadewright-azure](https://github.com/pkalin/shadewright-azure),
  [shadewright-fleet](https://github.com/pkalin/shadewright-fleet)) live in this repository. Tasks labelled
  `agent-ready` are written so that anyone, a person or a coding agent, can take one without further context.
- The roadmap is a public [GitHub project](https://github.com/users/pkalin/projects/2).
- The design is [docs/design.md](docs/design.md); decisions are [ADRs](docs/adr/index.md). Both are published
  at [shadewright.io](https://shadewright.io).
- [AGENTS.md](AGENTS.md) is the working guide: the invariants, the commit format, the checks to run, what docs
  a change must update. Read it before your first pull request; it applies to people and coding agents alike.

## Making a change

1. Start from an issue. Open one if none fits; say what you want to change and why.
2. One issue, one branch, one pull request with `Closes #<n>` in the description. Keep it small.
3. A change that weakens an [invariant](AGENTS.md#invariants-read-before-changing-anything), adds an action
   to the catalog, or alters an accepted decision starts with an ADR (status Proposed) in `docs/adr/`.
4. Run `make lint test` before opening the pull request, and `make api-docs` after a change to the API types.
   Add or update tests in the same pull request; a bug fix comes with a test that fails without it.
5. Pull requests get an automated review and a maintainer review. Expect questions; answer in the thread so
   the reasoning stays with the change.
6. Pull request titles follow Conventional Commits (see AGENTS.md); they become the commit messages on `main`.

## Reporting

- Bugs and ideas: an issue in this repository.
- Vulnerabilities: privately, as [SECURITY.md](SECURITY.md) describes. Never a public issue.
- Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Licence

Contributions are licensed under the [Apache License 2.0](LICENSE), like the rest of the project. There is no
contributor licence agreement to sign; opening a pull request is your statement that you may contribute the
change under that licence.
