---
title: Serve APIs Over MCP
linkTitle: MCP
weight: 105
description: Expose registered Restish APIs as MCP tools through the restish-mcp command plugin.
aliases:
  - /docs/plugins/mcp/
---

`restish-mcp` is a command plugin that exposes registered OpenAPI operations as
MCP tools. Use it when an MCP client should call APIs through Restish profiles,
auth, TLS, retries, and output normalization.

## Prerequisites

```bash
restish api connect example api.rest.sh 'prompt.api_key: docs-key'
restish plugin list
```

The API must be registered and have a usable spec.

## Serve Tools

```bash
restish mcp serve example
```

The plugin reads the registered API spec, turns operations into MCP tools, and
delegates HTTP execution back to Restish.

The generated help below is the exact command reference, including write-call
authorization, timeouts, and result-size limits. `tools/list` always reports
the complete OpenAPI operation inventory.

Tool arguments follow the OpenAPI parameter shape. Query arrays are sent as
repeated query keys when the spec uses the usual `form` plus `explode: true`
style, and header arrays are comma-joined. Object parameters and unsupported
array styles are rejected with a tool error so the client does not send
ambiguous values.

## Generated Plugin Help

<!-- BEGIN GENERATED: restish-docgen mcp-help -->
Generated from the compiled `restish-mcp` plugin binary.

### `restish mcp --help`

```text
Expose registered APIs as MCP tools via Restish-authenticated HTTP delegation.

Use `restish mcp serve <api...>` from an MCP client command configuration. Restish lists every OpenAPI operation and forwards authorized tool calls through the same auth, profile, TLS, and request pipeline as the CLI. Write calls remain disabled until `--allow-write-tools` is set; this execution gate never hides tools from discovery.

Usage:
  restish mcp [command]

Available Commands:
  serve    Serve registered APIs over stdio

Examples:
  restish mcp serve github
  restish mcp serve github --allow-write-tools

Flags:
  -h, --help   help for mcp

Use "restish mcp [command] --help" for more information about a command.
```

### `restish mcp serve --help`

```text
Serve registered APIs over the Model Context Protocol.

Discovery always lists every OpenAPI operation. By default, calls to POST, PUT, PATCH, and DELETE tools are rejected. Use `--allow-write-tools` only when an outer authorization boundary or trusted operator controls mutations; use `--read-only` to reject every call except GET and HEAD. Neither flag changes tools/list.

Usage:
  restish mcp serve [flags] <api...>

Examples:
  restish mcp serve github
  restish mcp serve github --allow-write-tools

Flags:
  --max-result-bytes int     Maximum tool result payload size
  --request-timeout int      Per-tool HTTP request timeout in seconds (0 disables)
  --read-only                Reject calls other than GET/HEAD operations
  --allow-write-tools        Permit calls to POST, PUT, PATCH, and DELETE operations
  -h, --help                 help for serve
```
<!-- END GENERATED -->

## Describe Operation Effects

Restish maps HTTP methods to standard MCP `readOnlyHint`, `idempotentHint`, and
`destructiveHint` annotations. Clients and outer authorization brokers can use
these descriptors when deciding whether a concrete call needs human approval.

`x-cli-ignore` and `x-mcp-ignore` remain available as source metadata but do not
remove an operation from `tools/list`. Tool visibility is not authorization.

## Good Fit

MCP works well for APIs with descriptions, schemas, and safe auth profiles.
Operations without `operationId` receive a stable method/path-derived name. It
is a poor fit when state-changing calls lack either trusted operator control or
an outer exact-call authorization broker.

OpenAPI parameters that use `content` keep their declared schema in MCP tools.
For JSON parameter content, pass the native object, array, or scalar value and
Restish serializes it into the outgoing HTTP parameter.

## Troubleshooting

- Run `restish api sync <name>` after spec changes.
- Confirm `restish <name> --help` shows generated operations.
- Use `restish plugin debug` when plugin startup or messages fail.
- If a write tool is listed but its call is rejected, opt in only when a trusted
  operator or outer authorization broker controls the concrete request.
- Use `--read-only` when every non-GET/HEAD call must be rejected.

## Related Pages

- [OpenAPI Reference](/docs/reference/openapi-cli-integration/)
- [Command Plugins](/docs/plugins/command-plugins/)
- [Plugin Messages](/docs/reference/plugin-messages/)
- [Troubleshooting](/docs/guides/troubleshooting/)
