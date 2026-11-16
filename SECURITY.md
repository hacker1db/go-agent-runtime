# Security policy

## Supported versions

Only the latest commit on `main` receives security fixes. The project is
pre-1.0.

## Reporting a vulnerability

Do not open a public issue. Report the vulnerability privately through
[GitHub private vulnerability reporting](https://github.com/hacker1db/go-agent-runtime/security/advisories/new).

Include affected code paths, reproduction steps, and the impact you expect.
You should receive an acknowledgement within 7 days.

## Security posture

- The library uses only the standard library. govulncheck runs on every push.
- CodeQL (`security-extended`, `security-and-quality`), gosec, dependency
  review, and OpenSSF Scorecard run in CI.
- CI uses no stored secrets. Workflows run with least-privilege
  `GITHUB_TOKEN` scopes and SHA-pinned actions. Scorecard publishes its
  results over OIDC.
- `FileStore` encrypts secret and scoped environment values at rest with
  AES-GCM. Supply the key from a KMS or secret manager, ideally one reached
  through workload identity.
- The examples show keyless model authentication with OIDC workload identity
  federation (`examples/06-oidc-workload-identity`).
