# OpenAPI contract policy

The deployed Custom GPT action contract is intentionally **frozen at OpenAPI contract version 0.5.3**.

`internal/agent/openapi.json` is a compatibility boundary, not a release-version file. Normal implementation, reliability, deployment, logging, timeout, performance, and documentation changes should **not** modify it.

Current frozen SHA-256:

```text
b703d50a1f9817bf537a6802bda4b83122e765450ab075e1d096086ab9ee3872
```

The service implementation version may advance independently of the OpenAPI contract version. A change to `openapi.json` must be treated as an explicit Action-contract migration because existing MyGPT installations may need their Action schema refreshed.

Before changing the contract, verify that the requirement cannot be implemented behind the existing four operations:

```text
POST /v1/command/run
POST /v1/command/start
GET  /v1/command/jobs/{id}
POST /v1/command/jobs/{id}/cancel
```

Prefer backward-compatible internal behavior, repository configuration, GPT instructions, and server-side implementation changes over adding or changing Action fields or operations.
