# 06 — OIDC workload identity federation

This example implements an `agentruntime.Provider` that calls a real model
without an API key, PAT, or client secret. The workload proves its identity
with an OIDC token that its platform issues. A security token service (STS)
exchanges that token for a short-lived access token. The provider caches the
access token and refreshes it before it expires.

```
platform OIDC token ──► STS (WIF) ──► short-lived access token ──► model API
(projected file or      RFC 8693 or     cached, refreshed           Bearer header
 GitHub Actions)        Entra assertion  2m before expiry
```

`agentruntime` never handles credentials. It talks only to the `Provider`.

## Run the demo

```sh
go run ./examples/06-oidc-workload-identity
```

With no configuration, the example starts local TLS fakes of the STS and the
model endpoint and writes a fake projected token to a temp file. Two model
calls share one token exchange.

## Use real endpoints

Set `WIF_TOKEN_URL` to switch to real mode. None of these values is a secret.

| Variable | Meaning |
|---|---|
| `WIF_TOKEN_URL` | STS or token endpoint (HTTPS required) |
| `WIF_STYLE` | `rfc8693` (default) or `client-assertion` |
| `WIF_AUDIENCE` | RFC 8693 `audience`, e.g. the GCP workload identity provider |
| `WIF_CLIENT_ID` | Entra application (client) ID for `client-assertion` |
| `WIF_SCOPE` | Requested scope |
| `WIF_SUBJECT_TOKEN_FILE` | Projected OIDC token file. Falls back to `AZURE_FEDERATED_TOKEN_FILE`, then to GitHub Actions OIDC |
| `WIF_OIDC_AUDIENCE` | Audience to request from GitHub Actions OIDC |
| `MODEL_BASE_URL` | OpenAI-compatible base URL (HTTPS), e.g. your model gateway |
| `MODEL_NAME` | Model identifier |

### Microsoft Entra (AKS workload identity, or GitHub Actions)

Add a federated credential to the app registration for your subject (a
Kubernetes service account or `repo:OWNER/REPO:ref:refs/heads/main`). Then
set:

```sh
WIF_STYLE=client-assertion
WIF_TOKEN_URL=https://login.microsoftonline.com/$TENANT_ID/oauth2/v2.0/token
WIF_CLIENT_ID=$APP_CLIENT_ID          # public identifier, not a secret
WIF_SCOPE=https://cognitiveservices.azure.com/.default
WIF_OIDC_AUDIENCE=api://AzureADTokenExchange   # GitHub Actions only
```

On AKS, the workload identity webhook sets `AZURE_FEDERATED_TOKEN_FILE`
automatically.

### Google Cloud Workload Identity Federation

```sh
WIF_STYLE=rfc8693
WIF_TOKEN_URL=https://sts.googleapis.com/v1/token
WIF_AUDIENCE=//iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$POOL/providers/$PROVIDER
WIF_SCOPE=https://www.googleapis.com/auth/cloud-platform
```

For APIs that require a service-account token, add the
`generateAccessToken` impersonation step after the exchange.

### GitHub Actions

```yaml
permissions:
  contents: read
  id-token: write   # lets the job mint an OIDC token; no stored secret
steps:
  - run: go run ./examples/06-oidc-workload-identity
    env:
      WIF_TOKEN_URL: ${{ vars.WIF_TOKEN_URL }}
      WIF_STYLE: client-assertion
      WIF_CLIENT_ID: ${{ vars.WIF_CLIENT_ID }}
      WIF_SCOPE: ${{ vars.WIF_SCOPE }}
      WIF_OIDC_AUDIENCE: api://AzureADTokenExchange
      MODEL_BASE_URL: ${{ vars.MODEL_BASE_URL }}
      MODEL_NAME: ${{ vars.MODEL_NAME }}
```

Store these values as repository variables (`vars`), not secrets, because none
of them is sensitive.

## Secure defaults in the code

- Token and model endpoints must use HTTPS. Plain HTTP is allowed only to
  loopback.
- The subject token is re-read on every exchange, so rotated tokens are
  picked up.
- Access tokens stay in memory only and are never logged.
- Error responses from the STS or model are not echoed, because they can
  reflect credentials. Response bodies are capped at 1 MiB.
- All requests take a `context` and use a client with a timeout.
