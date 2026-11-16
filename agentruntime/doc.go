// Package agentruntime implements the runtime layer of the Standard Agent
// Specification: it executes dual-sided agents on threads, assembles model
// context, calls providers, runs tools sequentially, enforces stop
// conditions, persists state before advancing, and coordinates parent and
// child subagent threads.
//
// The package is provider-agnostic. Callers supply a [Provider] for each
// model; credentials are the provider's concern. See the examples directory,
// including examples/06-oidc-workload-identity for a provider that uses
// short-lived federated credentials instead of static API keys.
package agentruntime
