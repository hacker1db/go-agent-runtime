# Contributing

## Quick start

```sh
make check   # tidy, fmt, vet, lint, race tests, vulncheck, run examples
```

Requirements: Go (the version in `go.mod` or newer),
[golangci-lint v2](https://golangci-lint.run/), and
[govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck).

## Coding standards

These rules are enforced in CI (`.golangci.yml`, `.github/workflows/ci.yml`).
A change merges only when every check passes.

### Formatting and layout

- Format code with `gofmt` and `goimports`. Imports from this module go in
  their own group after the standard library and third-party groups.
- `go mod tidy` must leave no diff.
- Keep the library dependency-free (standard library only). If a dependency
  seems necessary, open an issue first. Examples may not add dependencies
  either.

### API and documentation

- Every exported identifier has a doc comment that starts with its name.
- Add a `Runtime §N` reference when behavior implements a spec requirement.
  Update the conformance map in `README.md` when coverage changes.
- Error types are named `XxxError`. Sentinel errors are named `ErrXxx`.
- Check errors with `errors.Is` or `errors.As`, never with type assertions or
  `==` on wrapped errors. Wrap errors with `%w` and an `agentruntime:` prefix.

### Concurrency and context

- Functions that block or do I/O take `context.Context` as their first
  parameter and honor cancellation.
- Detached contexts (runs, effects, child threads) are deliberate. Add a
  comment wherever you create one.
- All tests run with `-race -shuffle=on`. A data race or an order-dependent
  test is a bug.

### Testing

- New behavior comes with tests. Bug fixes come with a test that fails without
  the fix.
- Tests use fakes (`ScriptedProvider`, `httptest`). They make no network calls
  outside loopback and need no credentials.
- Every example must run with `go run ./examples/<dir>` and no configuration.
  CI runs all of them.

## Security standards

- **No long-lived secrets.** Do not add API keys, PATs, or client secrets to
  code, examples, CI, or docs. Authenticate with OIDC or workload identity
  federation. See `examples/06-oidc-workload-identity`.
- **CI is least privilege.** Workflows declare `permissions: {}` at the top
  level, and each job opts in to only the scopes it needs. Pin actions to a
  full commit SHA with a version comment; Dependabot keeps the pins current.
  Check out code with `persist-credentials: false`.
- **Never interpolate untrusted event data** (PR titles, branch names, comment
  bodies) into `run:` steps. Pass it through `env:` instead.
- **Secure defaults in code.** Refuse plaintext HTTP to non-loopback hosts,
  bound how much of any response is read, never log or echo credentials or
  credential-bearing response bodies, and encrypt sensitive data at rest.
- `gosec`, CodeQL (`security-extended`), govulncheck, dependency review, and
  OpenSSF Scorecard all run in CI. Fix their findings. Suppress one only with
  a `//nolint:<linter> // <reason>` comment that explains why it is safe.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`,
  `fix:`, `docs:`, `ci:`, `chore:`, `refactor:`, `test:`, `build:`).
- Keep each change focused. Explain the reason in the commit body, not only
  what changed.
- Report vulnerabilities privately (see [SECURITY.md](SECURITY.md)), not in
  public issues.
