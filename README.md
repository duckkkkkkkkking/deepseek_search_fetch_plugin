# DeepSeek Search/Fetch Plugin for CLIProxyAPI

A native CLIProxyAPI plugin that adds server-side web search and page fetching to
models already exposed by the proxy.

The plugin injects two gateway-owned tools:

- `gateway_web_search` -> `POST https://api.deepseek.com/v1/tools/search`
- `gateway_web_fetch` -> `POST https://api.deepseek.com/v1/tools/fetch`

Clients do not install an MCP server for these tools. The plugin detects a
gateway-owned call, executes it with a server-side DeepSeek key, returns the
result to the selected model, and continues the request.

Client-owned tools are returned to the client unchanged. This includes function
tools, custom tools such as `apply_patch`, and their output items.

## Features

- Works on OpenAI Responses and custom OpenAI-compatible endpoints routed through
  CLIProxyAPI.
- Supports streaming and non-streaming requests.
- Keeps gateway-owned tool calls out of the public response.
- Preserves client function/custom tool calls and result round trips.
- Isolates continuation state by client authorization key and model.
- Persists continuation state on disk with configurable TTL and capacity eviction.
- Applies fetch-URL safety validation.
- Treats retrieved web content as untrusted source data.
- Explicitly refuses DeepSeek Pro routing and supports per-site model allowlists.

## Requirements

- CLIProxyAPI v8.0.10 or newer with native dynamic-library plugin support.
- Linux for the production `.so` artifact.
- Go 1.26 or newer to build.
- A DeepSeek API key with Search/Fetch API access.

## Build

```bash
make check
make build
```

The default output is `dist/deepseek-search.so`.

## Install

Create a private directory for the plugin, key, and settings:

```bash
install -d -m 0755 /opt/cliproxy-setup/deepseek-search-plugin/plugins
install -d -m 0700 /opt/cliproxy-setup/deepseek-search-plugin/keys
install -d -m 0700 /opt/cliproxy-setup/deepseek-search-plugin/settings
install -d -m 0700 /opt/cliproxy-setup/deepseek-search-plugin/state
install -m 0755 dist/deepseek-search.so \
  /opt/cliproxy-setup/deepseek-search-plugin/plugins/deepseek-search.so
```

Store the DeepSeek API key in a private key file:

```bash
install -m 0600 /dev/null /opt/cliproxy-setup/deepseek-search-plugin/keys/deepseek.key
chmod 0600 /opt/cliproxy-setup/deepseek-search-plugin/keys/deepseek.key
```

Write the key to that file using the server's secret-management workflow. Do not
put the key in this repository, `settings.json`, or `config.yaml`.

Copy [config.example.json](config.example.json) to a settings file and customize
the `models` list. The settings file contains only paths and non-secret policy;
it should still be mode `0600`.

Merge [cliproxy-plugin.example.yaml](cliproxy-plugin.example.yaml) into the
CLIProxyAPI `config.yaml` for the desired instance. Use a separate settings file
and key file for each proxy instance that has its own credential or model
allowlist.

Restart the target CLIProxyAPI service after installing or replacing the shared
library. Confirm the journal reports:

```text
plugin registered plugin_id=deepseek-search plugin_name=deepseek-search version=1.0.4
```

## Settings

| Field | Purpose |
| --- | --- |
| `key_file` | Absolute path to the DeepSeek key file; must be mode `0600` or stricter. |
| `api_base` | DeepSeek API base; production must be `https://api.deepseek.com`. |
| `models` | Client-facing model aliases handled by the plugin. |
| `excluded_models` | Explicit deny list. DeepSeek Pro is also rejected structurally. |
| `auto_choice_models` | Models that reject a forced named tool choice; gateway choices are adapted to `auto` with an explicit instruction. |
| `text_tool_call_models` | Models that emit a known textual `<tool_call>` format instead of structured tool calls. |
| `state_dir` | Absolute private directory for disk-backed continuation state. |
| `state_seconds` | Idle TTL for continuation state; defaults to 12 hours. |
| `state_max_bytes` | Total disk capacity for continuation state and aliases; defaults to 1 GiB. |
| `allow_test_api` | Test-only escape hatch for mock API servers; keep `false` in production. |

An individual request can opt out with `tool_choice: "none"` or top-level
metadata:

```json
{
  "metadata": {
    "deepseek_search": false
  }
}
```

## Security Notes

- The DeepSeek key is read from a private file and is never copied into plugin
  configuration, logs, responses, or this repository.
- Search/fetch requests use CLIProxyAPI host HTTP callbacks, preserving the host's
  transport and logging controls.
- Retrieved content is wrapped with a marker stating that it is untrusted source
  data. The search/fetch tool descriptions also instruct models not to follow
  instructions embedded in retrieved pages.
- Literal loopback, private, link-local, multicast, and credential-bearing fetch
  URLs are rejected. Fetching itself is performed by the DeepSeek remote service.
- Continuation history is stored only in private `0600` files under `state_dir`.
  It is isolated by authorization key and model, expires after the configured
  idle TTL, and evicts the oldest entries when the disk capacity is exceeded.
- Production configs should use a dedicated DeepSeek key with appropriate
  spending limits and rotate it outside this repository.

## Verification

The test suite covers:

- search/fetch continuation;
- streaming and non-streaming Responses/Chat paths;
- native custom tools and opaque reasoning items;
- mixed client/server tool round trips;
- tenant isolation;
- DeepSeek Pro exclusion;
- URL policy and Unicode query validation;
- text-form tool-call compatibility;
- unbounded continuation rounds and tool calls;
- disk persistence, 12-hour expiry, ownership isolation, and capacity eviction;
- split/native/vendor SSE parsing;
- concurrent access under Go's race detector.

```bash
make check
```

Live production checks should call search-only and fetch-only requests separately
and verify the host audit log records `gateway_web_search` or `gateway_web_fetch`.
