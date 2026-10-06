# HOME Data Agent

A deterministic Python Microsoft Agent Framework Agent exposing
`POST /v1/responses` and `GET /readyz`. It does not invoke a remote model.
Each query is saved as one JSON-escaped UTF-8 record in `$HOME/history.txt`.
The response shows the previous five queries, excluding the current query.
`FOUNDRY_SESSION_ID` is required and must be a UUID.

## Build and test

```bash
python3 -m venv /tmp/substrate-data-agent
/tmp/substrate-data-agent/bin/pip install -r demos/data-agent/requirements.lock
/tmp/substrate-data-agent/bin/python -m pytest demos/data-agent/test_app.py -q
/tmp/substrate-data-agent/bin/pip download --only-binary=:all: \
  --dest /tmp/substrate-data-agent-wheels -r demos/data-agent/requirements.lock
docker build --build-context wheels=/tmp/substrate-data-agent-wheels \
  -t "$ACR/data-agent:$VERSION" demos/data-agent
docker push "$ACR/data-agent:$VERSION"
```

The wheel context makes installation during the Docker build offline. The lock
includes the packages used by the tests; the wheels are downloaded with TLS
verification on the build host. Use the pushed manifest digest in
`DATA_AGENT_IMAGE`, not a mutable tag.

## Actor template

Render `workerpool.yaml.tmpl` with `MICROVM_WORKER_IMAGE` and
`SUBSTRATE_VERSION`. Render `actor-template.yaml.tmpl` with `DATA_AGENT_IMAGE`.
The cold-start profile automatically supplies one `home` durable-dir volume at
`/home/agent`, sets both snapshot scopes to Data, and injects the persisted
Actor UID as `FOUNDRY_SESSION_ID`. Image/template values cannot shadow that
system-managed environment value. A recreated Actor gets a new UUID.

Wait for `status.imagePreloadStatus.ready` and inspect its desired/cached node
lists before activating the demo. Existing Full/golden templates are unchanged.
Preload pins expire and are renewed by template reconciliation. A newly eligible
node is warmed on a subsequent pass; on-demand pulls remain the correctness
fallback, not the accepted warm-start performance path.

Suspend captures only HOME. The external manifest is committed after Azure Blob
uploads complete. Resume reconstructs HOME and cold-starts the process with the
same session ID. A node-local cache can serve the same immutable Data snapshot
without Blob reads; a first cross-node restore still needs the Blob download.

## Azure development validation

Use the existing resources and security constraints in `resources.dev.md`.
The Azure bootstrap environment is:

```text
ATE_STORAGE_BACKEND=azure
ATE_AZURE_STORAGE_ENDPOINT=https://menxiaosubstrate1005.blob.core.windows.net/
ATE_AZURE_STORAGE_CLIENT_ID=<Blob UAMI client ID>
ATE_AZURE_POSTGRES_CLIENT_ID=<PostgreSQL UAMI client ID>
ATE_AZURE_ACR_ENDPOINT=https://menxiaosubstrate1005.azurecr.io
ATE_AZURE_ACR_CLIENT_ID=<dedicated AcrPull UAMI client ID>
```

Federate the Blob identity to the actual atelet and ate-api-server
ServiceAccounts, the PostgreSQL identity to ate-api-server, and the ACR pull
identity to atelet. The Azure dev overlay explicitly projects the short-lived
SA token and sets WI environment variables: the installed AKS WI mutating
webhook drops the new v1 PodCertificate projection fields on opted-in Pods.
Native ateapi/atelet therefore do not opt into that mutation. The token audience,
federation and SDK authentication are unchanged; native mTLS is retained.
One Pod can exchange its projected token for multiple explicitly federated
identities; their client IDs must be selected independently. Never put an
access token into a permanent DSN or Secret. PostgreSQL token acquisition runs
for new runtime, owner, and watch-pool connections, retaining verified TLS.

Before installing Substrate, inspect the cluster certificate API discovery:

```bash
kubectl api-resources --api-group=certificates.k8s.io
```

The native installation requires both `ClusterTrustBundle` and
`PodCertificateRequest`. After the supplied AKS was upgraded to 1.37.0, both
were available as v1 and native installation/lifecycle tests passed. KVM is
present on all three nodes. Do not bypass mTLS or describe ordinary Kubernetes
Pod tests as native Actor suspend/resume E2E.

The environment-specific installer profile is selected with
`ATE_INSTALL_AZURE_DEV=true`; use the prebuilt ACR components, external Azure
PostgreSQL DSNs with `sslmode=verify-full`, and `--atenet-dataplane agentgateway`.
See `azure-verification.md` for the exact evidence, compatibility measures,
small-sample timings and retained negative-test cleanup limitation.

The independent Azure adapter/cache probes are in `tools/validate-azure`.
For a running Agent whose HOME has no prior queries, verify the real Responses
endpoint using:

```bash
/tmp/substrate-data-agent/bin/python demos/data-agent/verify.py \
  --url http://127.0.0.1:18080 --session-id <FOUNDRY_SESSION_ID>
```

This writes eight queries and checks the exact response format through the
OpenAI Python SDK. A process boot UUID is exposed in readiness and response
headers for cold-restart checks without changing the requested response text.

`verify-native.py` verifies actual Actors through a localhost CONNECT
port-forward to the atenet-router Service's 8081 port. It expects fresh
`native-a`/`native-b` query histories for its default scenario, and other workers
occupied so the same-node assertion is deterministic. `--benchmark-cycles N`
does not append history and collects bounded cold Data cycle samples. CLI times
include authentication/port-forward overhead; use atelet phase logs for runtime
measurements. Cross-node placement uses the authenticated native DrainWorker
action in `tools/validate-azure`, never direct database updates.
