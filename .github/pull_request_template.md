## What and why

<!-- Link the issue: Closes #123 / Refs #123 -->

## Checklist

- [ ] `make check` passes locally
- [ ] Tests cover the change (bug fixes include a failing-before test)
- [ ] Exported identifiers are documented; README conformance map updated if spec coverage changed
- [ ] No long-lived secrets (API keys, PATs, client secrets) introduced — use OIDC / workload identity
- [ ] New workflow steps use least-privilege `permissions:` and SHA-pinned actions
