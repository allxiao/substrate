# Azure Substrate Development Resources

Provisioned and verified on **2026-10-05**. This is a development/test environment,
not a production deployment. Resource configuration can drift; recheck Azure
before changing or relying on it. This document contains no credentials.

## Environment

| Setting | Value |
| --- | --- |
| Subscription | `edd0c578-a7c3-4a61-9536-63273eb9bc9b` |
| Subscription name | `Foundry Agents (Jun's team) - Test` |
| Entra tenant | `72f988bf-86f1-41af-91ab-2d7cd011db47` |
| Resource group | `menxiao-substrate-1005` |
| Region | `westus2` (West US 2) |
| AKS-managed node resource group | `menxiao-substrate-1005-nodes` |

All explicitly provisioned resources are in `menxiao-substrate-1005`. AKS places
its node VMs, disks, kubelet identity, load balancer, and outbound public IP in
the separate managed node resource group. Do not delete or manually modify
those managed resources to maintain the cluster.

## Resource Inventory

| Resource | Name | Configuration |
| --- | --- | --- |
| AKS | `menxiao-substrate-1005-aks` | Upgraded by the user to Kubernetes `1.37.0` on 2026-10-06; Free control-plane tier; managed Entra authentication and Azure RBAC |
| Node pool | `system` | 3 Ready nodes; `Standard_D4s_v5` (4 vCPU, 16 GiB RAM); autoscaler min 3 / max 6; zones 1, 2, 3; Ubuntu 24.04; 128 GiB OS disks |
| ACR | `menxiaosubstrate1005` | Basic; `menxiaosubstrate1005.azurecr.io`; admin account disabled; public endpoint enabled; `LegacyRegistryPermissions` role mode |
| PostgreSQL Flexible Server | `menxiao-substrate-1005-pg` | PostgreSQL 16; Burstable `Standard_B2s` (2 vCPU, 4 GiB RAM); 32 GiB Premium LRS storage; auto-grow enabled; 7-day backups; no HA or geo-redundant backup |
| PostgreSQL UAMI | `menxiao-substrate-1005-postgres` | Workload Identity federation; nonadministrator database login |
| Storage account | `menxiaosubstrate1005` | StorageV2; Standard LRS; HTTPS only; minimum TLS 1.2; Shared Key, public Blob access, and public network access disabled |
| Blob container | `actor-state` | Private container for future actor persistence |
| Blob UAMI | `menxiao-substrate-1005-blob` | Workload Identity federation; container-scoped Blob data permissions |
| VNet | `menxiao-substrate-1005-vnet` | `10.42.0.0/16` |
| Blob private endpoint | `menxiao-substrate-1005-blob-pe` | Approved; Blob private IP `10.42.5.4` |

The resource group ARM ID is:

```text
/subscriptions/edd0c578-a7c3-4a61-9536-63273eb9bc9b/resourceGroups/menxiao-substrate-1005
```

VM quota was checked **before creation**: West US 2 total regional vCPU usage
was `182/300`, and Standard DSv5 family usage was `44/100`. The initial nodes
require 12 vCPUs; six nodes require 24 vCPUs, plus upgrade surge capacity.
These are historical values, not a reservation or a current availability
guarantee. Check both quotas and SKU availability before expanding the pool.

## Network Boundaries

| Network setting | Value |
| --- | --- |
| AKS node subnet | `10.42.0.0/22`, subnet `aks` |
| PostgreSQL subnet | `10.42.4.0/24`, subnet `postgres`; delegated to `Microsoft.DBforPostgreSQL/flexibleServers` |
| Private endpoint subnet | `10.42.5.0/24`, subnet `private-endpoints` |
| AKS networking | Azure CNI Overlay; IPv4; network dataplane `azure` |
| Pod CIDR | `10.244.0.0/16` |
| Service CIDR | `10.43.0.0/16` |
| Cluster DNS service IP | `10.43.0.10` |
| Egress | Standard Load Balancer with one managed outbound public IP |
| Network policy provider | `none` |
| PostgreSQL private DNS zone | `substrate.private.postgres.database.azure.com` |
| Blob private DNS zone | `privatelink.blob.core.windows.net` |

Both private DNS zones are linked to the VNet. The Blob private endpoint has
its DNS zone group configured.

- The AKS API endpoint is public, not a private-cluster endpoint. No API
  authorized IP range restriction was configured. Access still requires
  Entra authentication and Kubernetes authorization.
- ACR is reachable through its public endpoint; it does not allow anonymous
  pulls. Basic ACR has no private endpoint in this environment.
- PostgreSQL and Blob public network access is disabled. A local Azure CLI
  login or an RBAC grant alone does **not** provide network access to them.
  Use a Pod inside this cluster, or a host with appropriate VNet connectivity
  and private DNS resolution.
- Use the normal service hostnames, not hardcoded private IPs. This preserves
  TLS hostname verification and tolerates endpoint changes.
- Workload Identity requires outbound access to Entra token endpoints;
  image pulls require outbound access to ACR. Preserve these paths if adding
  egress restrictions.
- No network-policy enforcement was enabled. Do not treat the namespace as
  a network isolation boundary or describe this as production hardened.

## AKS Authentication and ACR Pulls

Local AKS accounts are disabled. Do not request `--admin` credentials, create
long-lived Kubernetes credentials, or enable local accounts as a workaround.
The provisioner's Entra user has `Azure Kubernetes Service RBAC Cluster Admin`
at this cluster's scope; other users need their own appropriate access grants.

To connect without replacing the user's normal kubeconfig:

```bash
export AZURE_SUBSCRIPTION_ID=edd0c578-a7c3-4a61-9536-63273eb9bc9b
export AZURE_RESOURCE_GROUP=menxiao-substrate-1005
export KUBECONFIG="${HOME}/.kube/menxiao-substrate-1005.yaml"
mkdir -p "${HOME}/.kube"
az aks get-credentials \
  --subscription "$AZURE_SUBSCRIPTION_ID" \
  --resource-group "$AZURE_RESOURCE_GROUP" \
  --name menxiao-substrate-1005-aks \
  --file "$KUBECONFIG"
kubelogin convert-kubeconfig --kubeconfig "$KUBECONFIG" -l azurecli
kubectl get nodes -o wide
```

The kubelet UAMI is AKS-managed:

| Identifier | Value |
| --- | --- |
| Name | `menxiao-substrate-1005-aks-agentpool` |
| Client ID | `31f55b97-9bb7-4475-ac8f-284d898abd2b` |
| Principal/object ID | `b7c45d45-f661-4f02-ba70-694ce3b2b859` |
| Resource group | `menxiao-substrate-1005-nodes` |

It has `AcrPull` on the `menxiaosubstrate1005` registry. Normal Kubernetes
container image pulls therefore do not require an `imagePullSecret` or a
workload UAMI. This does not grant image **push** permissions to developers,
nor does it prove Substrate's own actor-image loading path uses the kubelet
credential flow; inspect that path when deploying actor workloads.

## Workload Identity Bindings

AKS OIDC issuer and Workload Identity are enabled. Issuer URL, including its
trailing slash:

```text
https://westus2.oic.prod-aks.azure.com/72f988bf-86f1-41af-91ab-2d7cd011db47/c6ac05b9-0f80-40fb-b583-7b20a577ba80/
```

Both bindings use tenant `72f988bf-86f1-41af-91ab-2d7cd011db47` and audience
`api://AzureADTokenExchange`.

| Setting | PostgreSQL identity | Blob identity |
| --- | --- | --- |
| UAMI name | `menxiao-substrate-1005-postgres` | `menxiao-substrate-1005-blob` |
| Client ID | `e7b80ffc-88e7-4b5f-a355-b91f2dfa14d2` | `e97ebd23-314e-4a01-8a89-54d36da6c26f` |
| Principal/object ID | `5c9811e4-7998-4e75-a7c8-a22ea4650d36` | `5549466a-59ca-4e1f-bc4c-b423b307cbc5` |
| Namespace | `ate-system` | `ate-system` |
| ServiceAccount | `substrate-postgres` | `substrate-blob` |
| Federated credential | `aks-ate-system-substrate-postgres` | `aks-ate-system-substrate-blob` |
| Subject | `system:serviceaccount:ate-system:substrate-postgres` | `system:serviceaccount:ate-system:substrate-blob` |

These ServiceAccounts already exist. Each has the appropriate
`azure.workload.identity/client-id` and `azure.workload.identity/tenant-id`
annotations. Application Pod templates must use the matching
`serviceAccountName` and have the label `azure.workload.identity/use: "true"`
on **Pod metadata**, not just Deployment metadata. The webhook injects the
projected token file and Azure identity environment variables.

Renaming the namespace or ServiceAccount requires a corresponding federation
change. A Pod has only one ServiceAccount; these two bindings do not
automatically let one Pod use both UAMIs. If an application needs both,
configure explicit federation for that application's ServiceAccount and
explicit client-ID selection. Do not broaden grants merely to bypass this.

## PostgreSQL Access and Substrate Configuration

| Setting | Value |
| --- | --- |
| Host | `menxiao-substrate-1005-pg.postgres.database.azure.com` |
| Port | `5432` |
| Database | `substrate` |
| Schema | `substrate` |
| UAMI login role | `menxiao-substrate-1005-postgres` |
| Schema/database owner role | `substrate_owner` (NOLOGIN) |
| Runtime read/write role | `substrate_readwrite` (NOLOGIN) |
| Entra administrator | `menxiao@microsoft.com` |
| Administrator object ID | `4e2863ff-debe-42e6-a348-78e745b75526` |
| Token resource | `https://ossrdbms-aad.database.windows.net` |
| Azure Identity SDK token scope | `https://ossrdbms-aad.database.windows.net/.default` |

Password authentication is disabled. The UAMI is mapped to a PostgreSQL
service principal using its **principal/object ID**, not its client ID, with
`pg_catalog.pgaadauth_create_principal_with_oid(..., 'service', false, false)`.
This explicit mapping avoids name resolution through Microsoft Graph. Azure
resource-management RBAC alone does not grant SQL access.

The UAMI has membership in both `substrate_owner` and `substrate_readwrite` so
the same test-environment login can perform migrations and runtime work. It is
**not** an Entra PostgreSQL administrator. For stronger production isolation,
use separate identities for migration ownership and runtime DML.

The application database no longer grants access to PUBLIC. The schema is
owned by `substrate_owner`; `substrate_readwrite` has schema USAGE. Owner
default privileges grant runtime access to newly created tables, sequences,
routines, and types. PUBLIC cannot create objects in the `public` schema.

Use `sslmode=verify-full` with a trusted CA bundle. The noncredential settings
for Substrate are:

```text
ATE_API_POSTGRES_SCHEMA=substrate
ATE_API_POSTGRES_OWNER_ROLE=substrate_owner
ATE_API_POSTGRES_READ_WRITE_ROLE=substrate_readwrite
```

Both connection strings should target the database and UAMI login above; the
owner/runtime role is selected separately by Substrate. See
[docs/postgres.md](docs/postgres.md) for the connection-string configuration.

**Application integration is still required:** the current pool setup in
[atepg.go](cmd/ateapi/internal/store/atepg/atepg.go) refreshes TLS material but
does not acquire or refresh Entra access tokens. Workload Identity federation
alone does not make that client passwordless. Implement token acquisition and
refresh for new connections across owner, runtime, and watch connections
before expecting Substrate to run reliably. Do not store a one-time token in
a DSN, a checked-in file, or a Kubernetes Secret as a permanent solution.
Tokens are short-lived; acquiring one only at process startup is insufficient.

Azure's `pgaadauth_*` management functions are available in the `postgres`
management database, not the `substrate` application database. Inspect the
function's actual returned columns rather than assuming documentation column
names match the server version.

## Blob Access

Endpoint: `https://menxiaosubstrate1005.blob.core.windows.net/`.
Container: `actor-state`.

The Blob UAMI has `Storage Blob Data Contributor` at exactly this ARM scope:

```text
/subscriptions/edd0c578-a7c3-4a61-9536-63273eb9bc9b/resourceGroups/menxiao-substrate-1005/providers/Microsoft.Storage/storageAccounts/menxiaosubstrate1005/blobServices/default/containers/actor-state
```

This permits container Blob reads/writes/deletes, not account-wide access.
Use an Azure SDK credential supporting Workload Identity, or Azure CLI
`--auth-mode login` after explicitly authenticating as the federated UAMI.
The Azure CLI does not automatically log in solely because the webhook
injected environment variables. Blob SDK token scope is
`https://storage.azure.com/.default`.

Do not use account keys, key-based connection strings, or key-signed SAS.
Shared Key is disabled. This is an Azure Blob service, **not** an S3-compatible
endpoint; provisioning it does not implement a Substrate storage backend or
configure actor snapshot persistence. Verify application/backend support
before wiring it into actor persistence.

## Company Authentication Constraints

- Do not request a Microsoft Graph token for scripts or perform preliminary
  `az ad` lookups. The company's policy blocks personal-account Graph tokens.
- Use known object IDs and explicit principal types for direct Azure RBAC
  operations, such as `--assignee-object-id` and
  `--assignee-principal-type ServicePrincipal` (or `User`).
- Keep ARM authorization, SQL authorization, and Blob data authorization
  distinct. A successful role assignment does not prove data-plane access.
- If a direct operation is denied, retain its error and request the specific
  required grant or policy-approved administrator action. Do not fall back to
  plaintext credentials or silently weaken authentication/network controls.
- Provisioning and verification used ARM, database, and federated identity
  tokens as needed; no Microsoft Graph token was requested. No passwords,
  image pull Secrets, or Storage keys were provisioned by this setup.

## Verified State and Remaining Work

Verified on 2026-10-05:

- All three nodes were Ready, with autoscaler configuration min 3 / max 6.
  Actual scale-out/scale-in under load was not exercised.
- Two validation Pods pulled a private ACR image with no `imagePullSecrets`.
- Both Pod ServiceAccounts received the correct Workload Identity injection
  and successfully exchanged their projected tokens.
- The PostgreSQL UAMI connected with certificate/hostname verification and
  TLS 1.3, created a table as owner, then inserted, selected, updated, and
  deleted data as the runtime role. The test transaction was rolled back.
- The Blob UAMI uploaded, downloaded, verified content, and deleted a test
  Blob through the private endpoint.
- Validation Pods, the temporary ACR image/repository, and temporary SQL were
  removed. No Kubernetes Secrets were left in `ate-system`. The two
  ServiceAccounts, federated credentials, and resource permissions remain.

**At initial provisioning, Substrate had not been deployed.** No Substrate component images,
gVisor/runtime setup, worker pools, ingress, or actor persistence integration
were installed by this provisioning. ACR was empty after validation cleanup.
Do not present this environment as an end-to-end working Substrate deployment.
The 2026-10-06 native deployment and lifecycle validation below supersede that
initial-provisioning statement.

During deployment, follow [README.md](README.md), including node labeling for
`ate.dev/substrate-version`. Autoscaled nodes require the appropriate version
label too; otherwise they do not host Substrate workers. Use the installed
build version, not an invented value, and plan labeling for future nodes.

Resources incur ongoing charges. Burstable PostgreSQL, no database HA,
single-region LRS storage, and the AKS Free tier are development choices.
Obtain approval before changing authentication policy, expanding exposure,
performing destructive cleanup, or materially expanding resource scope.

## HOME Data Implementation Validation (2026-10-05)

The cold HOME/Data implementation and standalone adapters were exercised against
these existing resources. This does **not** supersede the note above that a native
Substrate deployment has not been completed.

- All three existing nodes expose `/dev/kvm` and run kernel `6.8.0-1067-azure`.
  No new node pool, VM, database, Storage account or public endpoint was created.
- The native Substrate deployment is blocked: AKS discovery exposes only
  `CertificateSigningRequest`, not `ClusterTrustBundle` or `PodCertificateRequest`.
  `/apis/certificates.k8s.io/v1beta1` returns NotFound. Do not bypass mTLS or
  confuse ordinary Pod tests with native Actor suspend/resume validation.
- Blob Workload Identity through the private endpoint passed streaming upload,
  download, listing, OAuth-authenticated service-side block copy, and idempotent
  deletion. The actual tar/zstd helpers passed HOME-only upload, manifest-last
  commit, download and extraction; a non-HOME sentinel was excluded.
- PostgreSQL Workload Identity passed fresh connections with verified TLS and
  runtime/owner role selection. The implementation refreshes tokens for newly
  opened runtime, owner and watch-pool connections; the pool hook has unit tests.
- The final Agent image passed seven non-streaming and one streaming Responses
  request through the OpenAI Python SDK in AKS, with the requested deterministic
  formatting and history excluding the current query.
- The Agent OCI image was pulled through the new custom WI ACR keychain and
  cached under `/var/lib/ate/image-cache` on all three nodes, independently of
  kubelet/containerd image caching. Initial cold fills took about 4.0-4.6 seconds;
  fresh processes on warm nodes took about 5-8 milliseconds, with repeated
  digest lookups about 0.1-0.25 milliseconds. These are image-cache timings,
  **not** Actor activation or cross-node resume measurements.

Published artifacts retained in the existing ACR:

```text
menxiaosubstrate1005.azurecr.io/data-agent@sha256:b584c4fc70b73ae25dd2d084ecf94a8e1b3cb6703dfadda971d010f02bbc1e7a
menxiaosubstrate1005.azurecr.io/azure-validate@sha256:090e794da6bef252993b603f63201455b39904e1214c0b71de8a7f4cd52bd697
```

One additional UAMI was created in `menxiao-substrate-1005` for custom actor
image pulls, with only `AcrPull` on this ACR:

| Setting | Value |
| --- | --- |
| UAMI | `menxiao-substrate-1005-acr-pull` |
| Client ID | `b2dd1f5d-5581-4d07-9f2d-d27c4c9d1384` |
| Principal ID | `5fcf4a92-4d7d-4480-aa5b-82328f44a1ff` |
| Federation | `aks-ate-system-substrate-acr` |
| Subject | `system:serviceaccount:ate-system:substrate-acr` |

The annotated `substrate-acr` ServiceAccount is retained. Its new federation
initially returned AADSTS70025 on one node during propagation; a targeted retry
succeeded without changing permissions. The existing Blob/PostgreSQL identity
grants were not broadened. Future native deployment still needs explicit
federations for the real atelet/ate-api-server ServiceAccounts.

All temporary validation/debug Pods and localhost port-forwards were cleaned
up. The validation Blob prefixes were deleted by the verifier. ACR artifacts,
the image caches, and the three federated ServiceAccounts are retained.
See [the detailed report](demos/data-agent/azure-verification.md).

## Native AKS 1.37.0 Validation (2026-10-06)

The user upgraded the existing AKS control plane and all three system nodes to
1.37.0. Live discovery now serves both `ClusterTrustBundle` and
`PodCertificateRequest` in `certificates.k8s.io/v1`. Kernel remains
`6.8.0-1067-azure`, and atelet advertises `ate.dev/kvm` on all three nodes.

Native Substrate build `azure-data-20261006` is deployed and retained:
ate-api-server 2/2, ate-controller 1/1, atenet-router 1/1, atenet-egress 1/1,
atelet DaemonSet 3/3, the original podcertificate controller, and three
microVM WorkerPool Pods (one per node). The original signer, certificate trust
and mTLS are active; Azure PostgreSQL and Blob remain private and passwordless.
No new node pool, AKS, VM, database, account or public endpoint was created.

The existing UAMIs received only additional actual-component federations:

| Identity | Added subjects |
| --- | --- |
| `menxiao-substrate-1005-blob` | `system:serviceaccount:ate-system:ate-api-server`, `system:serviceaccount:ate-system:atelet` |
| `menxiao-substrate-1005-postgres` | `system:serviceaccount:ate-system:ate-api-server` |
| `menxiao-substrate-1005-acr-pull` | `system:serviceaccount:ate-system:atelet` |

Existing role grants were not widened. The installed AKS WI mutating webhook
was observed to drop v1 `podCertificate` projections while injecting WI.
The environment-specific native overlay therefore opts those two component
Pods out of that mutation and explicitly projects kubelet-rotated one-hour
tokens for audience `api://AzureADTokenExchange` with matching WI SDK
environment/client IDs. No static credential or authentication bypass is used.

The `microvm` SandboxConfig uses content-addressed assets under
`azblob://actor-state/runtime/amd64/<sha256>/<filename>`; all four source and
remote hashes matched the repository pins. These runtime files are retained.
Component images are in the existing ACR under `substrate/`, tag
`azure-data-20261006`, and installed by digest. The existing pinned
agentgateway image was mirrored to that ACR. The template's Actor image remains
`data-agent@sha256:b584c4fc70b73ae25dd2d084ecf94a8e1b3cb6703dfadda971d010f02bbc1e7a`.

The real `ate-demo-data-agent/data-agent` cold template has no golden tag and
reports all three atelet nodes cached. Real native tests passed Responses/SSE,
isolation, same-node cold Data resume, forced node-0-to-node-2 resume, stable
session UUID with new boot UUID, same-name recreation with empty HOME, rootfs
state exclusion, and upload-permission-denied fail-closed behavior. Final
`native-a` snapshot inspection found exactly Data tar/manifest, only HOME,
eleven historical records, and no VM memory files.

Five tiny-HOME warm-cache measurements yielded atelet Resume p50 5.627 s,
sample p95 5.655 s; Suspend p50 83.7 ms, sample p95 90.5 ms. Every measured
restore logged a local Data cache hit. Python Agent readiness dominates.
These are five smoke samples, not a production SLA or sub-500 ms result.

Normal retained Actors `native-a`, `native-b`, `native-c` are safely suspended.
The drained empty worker was replaced and all three workers are Active.
Temporary upload/inspection Pods and localhost CONNECT tunnel were cleaned.
Negative test `native-upload-denied`/`data-agent-upload-denied` remains because
DeleteActor also receives 403 when listing the unauthorized container; the
Actor is DELETING, with no committed externalSnapshot. Permissions were not
expanded and no direct database cleanup was performed to conceal this boundary.

The latest native validator is published as:

```text
menxiaosubstrate1005.azurecr.io/azure-validate@sha256:d5b29e7677b4436484e89365393a95e02d663977453f6cd2f88db70405bee2c4
menxiaosubstrate1005.azurecr.io/runtime-assets@sha256:105cb66be1354601cb8a31bf3c4917316e35a91822c957e0985601f75e9fabd1
```

See [the updated native verification report](demos/data-agent/azure-verification.md)
for exact session/boot evidence, timing separation, compatibility measures and
the previously observed repository-wide verification limitations.
