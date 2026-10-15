# Agent Adapter SDK

Go and Python clients that let an agent framework read the portable context
envelope (§23.5) the compatibility gateway stores for a session.

Both clients call `GET /v1/envelope?session_id=<id>` on the compatibility
gateway with the caller's SCOS bearer token. The gateway takes tenant and
principal from the token's claims, so a client can only read envelopes in its
own tenant. The clients refuse an envelope recorded for a different session
or agent.

## Go

```go
cli, err := agentadaptersdk.NewClient(agentadaptersdk.AdapterConfig{
	AgentID:     "my-agent",
	Boundary:    agentadaptersdk.BoundaryHTTP,
	GatewayURL:  "https://compatibility-gateway:8087",
	BearerToken: token,
})
env, err := cli.FetchEnvelope(ctx, sessionID) // errors.Is(err, ErrEnvelopeNotFound) on 404
```

## Python

```python
from scos_agent import AgentAdapterClient, EnvelopeNotFound

client = AgentAdapterClient("my-agent", "https://compatibility-gateway:8087", token)
envelope = client.fetch_envelope(session_id)
```

Tests: `go test ./packages/agent-adapter-sdk/` and, from `python/`,
`PYTHONPATH=. python3 tests/test_client.py`.
