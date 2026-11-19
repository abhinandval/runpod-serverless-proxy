# Contributing

## Setup

Every runtime and tool is managed by [mise](https://mise.jdx.dev). Do not install
Go, Python, Node or anything else by hand for this repo.

```bash
mise trust && mise install
```

```bash
cp .env.example .env
```

mise loads `.env` automatically inside the repo. The project is polyglot by
design: when a new runtime is needed (for example Python for a worker shim),
add it to `[tools]` in `mise.toml` in the same commit that first uses it.

## Tasks

| Command | What it does |
| --- | --- |
| `mise run build` | Compile every package |
| `mise run test` | Run unit tests |
| `mise run lint` | `go vet` plus a formatting check |
| `mise run fmt` | Format Go sources |
| `mise run probe -- <command>` | Run the spike probe CLI |

New recurring commands belong in `mise.toml` as tasks, not in ad-hoc scripts.

## Commits

Commits are **atomic** and follow [Conventional Commits](https://www.conventionalcommits.org/).

**Atomic** means one logical change per commit:

- it builds and passes `mise run test` and `mise run lint` on its own;
- it can be reverted without taking unrelated work with it;
- refactors, behaviour changes and formatting are separate commits;
- a change and its tests go together.

**Format:**

```
type(scope): subject

optional body: what changed and why, wrapped at 72 columns

optional footers (BREAKING CHANGE: ..., Refs: ...)
```

- `subject`: imperative mood, lower case, no trailing period, at most 72 characters.
- `scope`: the area touched, e.g. `manage`, `infer`, `probe`, `gateway`, `fleet`,
  `images`, `docs`, `mise`. Omit it only for repo-wide changes.

| Type | Use for |
| --- | --- |
| `feat` | New user-visible capability |
| `fix` | Bug fix |
| `refactor` | Code change with no behaviour change |
| `perf` | Performance improvement |
| `test` | Tests only |
| `docs` | Documentation only |
| `build` | Build system, dependencies, images, mise config |
| `ci` | CI configuration |
| `chore` | Maintenance that fits nothing above |

Breaking changes use `!` after the type or scope and a `BREAKING CHANGE:` footer.

**Examples:**

```
feat(manage): add endpoint create, patch and delete
fix(infer): record time to first SSE data line, not first byte
test(manage): cover 429 retry with Retry-After
build(mise): pin go 1.26 and add lint task
docs(spike): record cold-start timings for load balancer endpoints
feat(fleet)!: rename unit `spec.gpu.pool` to `spec.gpu.pools`
```

Not acceptable: `update stuff`, `wip`, `fix review comments`, or one commit that
adds a feature, reformats three files and bumps a dependency.

## Branches and pull requests

- Branch from `main`: `type/short-description`, e.g. `feat/fleet-reconciler`.
- Keep pull requests small and focused; rebase onto `main` rather than merging it in.
- The pull request title follows the same Conventional Commits format.
- Describe what changed, why, and how it was verified (commands run, probe output).
- History on `main` stays linear: rebase-merge so atomic commits are preserved.

## Secrets and billable resources

- Keys live in `.env` or your shell, never in code, commits, issues or chat.
- Use scoped RunPod keys: a management key for fleet changes and a narrower key
  for inference.
- `probe up` creates billable endpoints and `probe down` deletes them. The tool
  only deletes endpoints it tagged (`managed-by=rsp-spike`) or recorded in
  `spike/state.json`. Always run `probe down` when a session ends.
- `spike/results/` and `spike/state.json` are git-ignored because they contain
  endpoint ids. Summarise results in `docs/spike-findings.md` instead.

## Code conventions

- Go: standard library first; add a dependency only when it clearly earns its place.
- `gofmt` formatting, `go vet` clean.
- Tests use `net/http/httptest` fakes; no test may call the real RunPod API.
- Exported identifiers get a doc comment; comments explain why, not what.
