# AWS BGP Infraprovider PoC — OCPSTRAT-3267

End-to-end test infrastructure for validating BGP routing with OpenShift
Virtualization on AWS. Tests VM egress source-IP preservation across live
migration using Route Server, Transit Gateway, and IPsec VPN.

Can run standalone (hand-rolled Route Server peering and FRR configuration)
or integrated with the
[bgp-cloud-connector](https://github.com/openshift/bgp-cloud-connector)
operator (`AWS_BGP_CLOUD_CONNECTOR=1`), which owns that same plumbing
declaratively instead. See [bgp-cloud-connector integration](#bgp-cloud-connector-integration).

## Architecture

```
┌─────────────────── On-Prem (local machine) ───────────────────┐
│                                                                │
│  ┌──────────────────────────────────────── bgpnet ──────────┐  │
│  │  172.29.0.0/24  (podman network, rootful)                │  │
│  │                                                          │  │
│  │  ┌───────────────┐        ┌────────────────────────┐     │  │
│  │  │ iperf container│        │ FRR container          │     │  │
│  │  │ 172.29.0.x     │        │ 172.29.0.2             │     │  │
│  │  │ (netshoot)     │        │ strongSwan + FRR       │     │  │
│  │  └───────────────┘        │   vti1 ── IPsec tun 0  │     │  │
│  │                            │   vti2 ── IPsec tun 1  │     │  │
│  │                            └──────────┬─────────────┘     │  │
│  └───────────────────────────────────────┼──────────────────┘  │
└──────────────────────────────────────────┼─────────────────────┘
                                           │ IPsec/IKEv2 (UDP 4500)
                                           │
┌──────────────────────── AWS VPC 10.0.0.0/16 ──────────────────┐
│                                                                │
│  ┌──────────────┐   eBGP    ┌──────────────┐                  │
│  │ Route Server │◄─────────►│ Worker nodes │                  │
│  │ (per-AZ      │           │ 10.0.x.x     │                  │
│  │  endpoints   │           │  ┌─────────┐ │                  │
│  │  in worker   │           │  │ OVN/VM  │ │                  │
│  │  subnets)    │           │  │10.x.x.x │ │                  │
│  └──────┬───────┘           │  └─────────┘ │                  │
│         │ propagates        └──────────────┘                  │
│         │ UDN routes                                          │
│         │ to VPC RTs                                          │
│  ┌──────┴───────┐                                             │
│  │Transit GW    │◄── TGW Connect ──► FRR (BGP over GRE)      │
│  │  + VPN att.  │◄── Site-to-Site VPN ──► strongSwan          │
│  └──────────────┘                                             │
│                                                                │
│  VPC Route Tables:                                             │
│    172.29.0.0/24 → TGW    (return path to on-prem)            │
│    10.0.0.0/8    → TGW    (static, for UDN subnets)           │
│    10.x.x.0/20   → RS     (propagated UDN routes)             │
└────────────────────────────────────────────────────────────────┘
```

## What it tests

The upstream kubevirt.go `kv-live-migration` test with `ingress: "routed"` validates:

1. **East/west iperf3** — VM ↔ test pods on UDN (~5 Gbits/sec)
2. **North/south ingress iperf3** — external container → VM (~280 Mbits/sec via VPN)
3. **North/south egress ICMP** — VM → external container (ping)
4. **North/south egress iperf3** — VM → external container (~280 Mbits/sec via VPN)
5. **Egress source-IP check** — external server log shows `connected to <VM-IP>`,
   proving no SNAT to node IP (the core OCPSTRAT-3267 requirement)
6. **Live migration** — VM migrates to another worker node
7. **Post-migration** — all of the above still work after migration

## Prerequisites

- AWS account with permissions for VPC, EC2, TGW, VPN, Route Server
- OpenShift 4.22+ cluster on AWS with:
  - CNV installed (kubevirt)
  - At least 2 worker nodes
  - Shared gateway mode (default, no `routingViaHost`)
  - **Standalone mode only** (`AWS_BGP_CLOUD_CONNECTOR` unset) — these must
    already be in place; the test does not configure them itself:
    - FRR-k8s deployed (`additionalRoutingCapabilities: FRR`)
    - Route advertisements enabled (`routeAdvertisements: Enabled`)
  - **`AWS_BGP_CLOUD_CONNECTOR=1` mode** — instead, the
    [bgp-cloud-connector](https://github.com/openshift/bgp-cloud-connector)
    operator must already be deployed (namespace
    `openshift-bgp-cloud-connector`); it configures the two items above
    itself as part of reconciling its `BGPCloudConfiguration` CR. See
    [bgp-cloud-connector integration](#bgp-cloud-connector-integration).
- `aws` CLI configured (`aws sts get-caller-identity` works)
- `podman` available locally (rootful, via sudo)
- `oc` / `kubectl` configured with cluster kubeconfig
- Static public IP recommended (for Customer Gateway stability)

## Usage

```bash
export KUBECONFIG=/path/to/kubeconfig

# First run — creates all AWS infra (takes ~5 min)
cd openshift
bash ./run-aws-bgp-test.sh

# Subsequent runs — reuse AWS infra (takes ~3 min)
export AWS_KEEP_INFRA=1
bash ./run-aws-bgp-test.sh

# Debug failures — pause test on failure for forensics
export AWS_KEEP_INFRA=1
export AWS_PAUSE_ON_FAILURE=30m
bash ./run-aws-bgp-test.sh

# With bgp-cloud-connector instead of hand-rolled FRR/peering (operator
# must already be deployed — see Prerequisites)
export AWS_BGP_CLOUD_CONNECTOR=1
bash ./run-aws-bgp-test.sh
```

### Detached execution (long runs)

```bash
: > /tmp/aws-bgp-test.log
export KUBECONFIG=/path/to/kubeconfig
export AWS_KEEP_INFRA=1
export AWS_PAUSE_ON_FAILURE=30m
cd openshift
nohup bash ./run-aws-bgp-test.sh > /tmp/aws-bgp-test.log 2>&1 &
disown

# Monitor
tail -f /tmp/aws-bgp-test.log
```

## Environment variables

| Variable | Description | Default |
|---|---|---|
| `KUBECONFIG` | Path to kubeconfig | `../kubeconfig` |
| `AWS_BGP_CLOUD_CONNECTOR=1` | Delegate Route Server peering, SourceDestCheck, FRRConfiguration, and the live-migration spec's CUDN/RouteAdvertisements to the [bgp-cloud-connector](https://github.com/openshift/bgp-cloud-connector) operator instead of creating them by hand. See [bgp-cloud-connector integration](#bgp-cloud-connector-integration) | (standalone, hand-rolled) |
| `AWS_KEEP_INFRA=1` | Persist AWS resources between runs | (destroy after test) |
| `AWS_PAUSE_ON_FAILURE=<duration>` | Pause on failure for forensics | (no pause) |
| `AWS_REGION` | AWS region override | (from cluster infra status) |
| `AWS_ONPREM_IP` | Public IP for Customer Gateway | (auto-detected via ifconfig.me) |
| `AWS_BGP_MACHINE_NETWORK_CIDR` | bgpnet CIDR | `172.29.0.0/24` |
| `CONTAINER_RUNTIME` | `podman` or `docker` | `podman` |
| `GINKGO_FOCUS` | Override test focus regex | routed L2 primary UDN live migration |

## Files

| File | Description |
|---|---|
| `run-aws-bgp-test.sh` | Test launcher script |
| `test/aws_bgp_test.go` | Go test entry point (TestAWSBGP) |
| `test/infraprovider/aws.go` | AWS provider: FRR container, strongSwan, BGP config. `ensureBGPCloudConnector()` applies `BGPCloudConfiguration`/`BGPRouting` when `AWS_BGP_CLOUD_CONNECTOR=1`; `createPerAZFRRConfigurations()` is the standalone-mode equivalent |
| `test/infraprovider/aws_cluster.go` | Cluster info discovery (VPC, workers, subnets, SGs) |
| `test/infraprovider/aws_infra.go` | AWS resource lifecycle (Route Server, TGW, VPN, SG rules). Route Server peer creation and SourceDestCheck disabling are skipped when `AWS_BGP_CLOUD_CONNECTOR=1` — the operator manages both dynamically |
| `test/infraprovider/aws_vpn.go` | VPN config parsing, swanctl template, xfrm interfaces |
| `test/infraprovider/openshift.go` | Provider interface (modified to support AWS fallback) |

`../../test/e2e/kubevirt.go` (vendored upstream test) also has a scoped,
env-gated patch for this integration — see
[bgp-cloud-connector integration](#bgp-cloud-connector-integration).

## Upstream test modifications

The `test/e2e/kubevirt.go` file has minimal modifications to support
single-stack IPv4 clusters (AWS IPI clusters are single-stack by default):

- `networkDataForCluster()` — returns IPv4-only cloud-init network data when
  `!isIPv6Supported`, preventing NetworkManager from tearing down the connection
  when DHCPv6 fails.
- `externalContainerIPs` — gates IPv4/IPv6 append on `isIPv4Supported` /
  `isIPv6Supported`, fixing ICMP ping to IPv6 addresses on single-stack clusters.

When `AWS_BGP_CLOUD_CONNECTOR=1`, `kubevirt.go` has one additional, narrowly
scoped patch — see [bgp-cloud-connector integration](#bgp-cloud-connector-integration).

## bgp-cloud-connector integration

Set `AWS_BGP_CLOUD_CONNECTOR=1` to replace this PoC's hand-rolled, static AWS
BGP peering/FRR setup with the
[bgp-cloud-connector](https://github.com/openshift/bgp-cloud-connector)
operator. The operator must already be deployed on the cluster (namespace
`openshift-bgp-cloud-connector`) — deploying it is out of scope for this
test; see its own [deployment docs](https://github.com/openshift/bgp-cloud-connector/blob/main/docs/deployment.md).

Everything below is additive and env-gated: with the variable unset, the
test behaves exactly as before (static per-AZ `FRRConfiguration`, static
Route Server peers, per-spec `ClusterUserDefinedNetwork`/`RouteAdvertisements`
with a random subnet).

### What moves to the operator

| Step | Standalone (default) | `AWS_BGP_CLOUD_CONNECTOR=1` |
|---|---|---|
| Enable FRR + routeAdvertisements | Manual prerequisite (must already be set before running the test) | `BGPCloudConfiguration` reconciliation patches `Network.operator.openshift.io/cluster` itself |
| Per-AZ FRR↔Route Server peering config | `createPerAZFRRConfigurations()` hand-writes one `FRRConfiguration` per AZ | `BGPCloudConfiguration` auto-discovers the Route Server's endpoints/ASN per AZ and generates the same CRs |
| Route Server peers (worker↔endpoint) | Created once, statically, in `createAWSInfrastructure()` | Created/removed dynamically by the operator, keyed off nodes matching `routerNodeSelector` |
| `SourceDestCheck` on worker ENIs | Disabled once, statically | Disabled dynamically by the operator for each matching node |
| `ClusterUserDefinedNetwork` + `RouteAdvertisements` for the live-migration spec | `kubevirt.go` creates a fresh one per spec run, with a random subnet (`randomCUDNSubnets()`) | A single, long-lived `BGPRouting` CR (network `aws-bgp-poc`, fixed subnet `10.200.0.0/16`) owns both; the spec labels its namespace and waits for adoption instead |

What's **unchanged** either way: Route Server/endpoint creation, Transit
Gateway, TGW Connect (GRE), Site-to-Site VPN, Customer Gateway, Security
Group rules, and the on-prem FRR container. The operator only *discovers*
an existing Route Server (via `spec.aws.routeServerIDs`) — it does not
provision one.

### What `ensureBGPCloudConnector()` does (in `aws.go`)

Runs once, during AWS infra setup (before any Ginkgo spec), right after the
Route Server and its per-AZ endpoints exist:

1. Labels every worker node discovered by `readAWSClusterInfo()` with
   `bgp_router=true` (the `routerNodeSelector` value used below).
2. Applies the singleton `BGPCloudConfiguration` (`platform: AWS`,
   `routerNodeSelector: {bgp_router: "true"}`, `bgp.localASN: 65001`,
   `aws.routeServerIDs: [<just-created Route Server ID>]`), then polls
   `status.phase` until `Ready` (or surfaces `status.conditions` and fails
   the test setup if it goes `Degraded` or times out after 10 minutes).
3. Applies the `BGPRouting` CR (`network.name: aws-bgp-poc`,
   `subnets: ["10.200.0.0/16"]`) and returns **without** waiting for it to
   reach `Ready` — see [why below](#why-bgprouting-is-applied-without-waiting).

### What the `kubevirt.go` patch does

Scoped to exactly the `topology: Layer2, role: Primary, ingress: "routed",
evpn: nil` entry in the shared `DescribeTable("should keep ip", ...)` (the
one matched by the default `GINKGO_FOCUS`) via a single
`useBGPCloudConnectorNetwork` boolean computed from `AWS_BGP_CLOUD_CONNECTOR`
and those four `testData` fields. All other entries in the table, and this
same entry with the variable unset, are unaffected:

- Namespace labels gain `cluster-udn: aws-bgp-poc` alongside the existing
  `k8s.ovn.org/primary-user-defined-network` label (both must be set at
  namespace creation — OCP admission policy forbids adding the UDN label
  later).
- `cidrIPv4`/`cidrIPv6` are set to the fixed `10.200.0.0/16` /
  `fd00:200::/64` instead of calling `randomCUDNSubnets()` (the second value
  is a throwaway, unused-but-valid CIDR so the unconditional FRR
  static-route-injection step later in the spec still has a non-empty value
  to work with — the cluster is single-stack IPv4, nothing actually
  advertises it).
- `createCUDN(cudn)` is skipped; instead the spec waits (up to 60s) for a
  `NetworkAttachmentDefinition` named `cluster-udn-aws-bgp-poc` to appear in
  its namespace, confirming the pre-existing, `BGPRouting`-owned CUDN
  adopted it.
- The per-spec `RouteAdvertisements` creation is skipped — the shared
  `bgp-cc-route-advertisements` the operator already created covers it.
  (These compose fine with each other at the `RouteAdvertisements` level
  regardless: OVN-Kubernetes's `clustermanager/routeadvertisements`
  controller explicitly skips other controllers' own
  `ovnk-generated-*`-prefixed output when choosing *source*
  `FRRConfiguration`s, specifically so multiple `RouteAdvertisements` can
  layer safely over the same peering sessions. The real reason a per-spec
  `BGPRouting` isn't viable here is one level up, at the CUDN: OVN-Kubernetes
  allows only one primary UDN per namespace, so a `BGPRouting`-owned CUDN
  and the test's own per-spec CUDN can't both claim it.)
- A `cudnName` variable (`bgpCloudConnectorCUDNName` or `cudn.Name`,
  depending on the path taken) replaces the handful of other `cudn.Name`
  reads later in the spec (`createIperfServerPods`, `getCUDNSubnets` x3,
  `podNetworkStatusByNetConfigPredicate`) that would otherwise nil-panic
  since `cudn` itself stays `nil` on the `bgp-cloud-connector` path.

### Why `BGPRouting` is applied without waiting

`BGPRouting`'s controller requires at least one namespace to already exist
with the required labels before it can create the CUDN (bgp-cloud-connector's
`reconciliation.md`, "Validate Namespace + Create CUDN" phase). No such
namespace exists yet when `ensureBGPCloudConnector()` runs — it's infra
setup, before any spec. Applying the CR and moving on means it's already in
place, reacting as soon as the spec creates and labels a matching namespace
a few seconds later (confirmed empirically: adoption — i.e. the NAD
appearing — takes well under the 60s the spec waits for it), rather than
having `ensureBGPCloudConnector()` block for a readiness condition it has no
way to satisfy yet.

### Naming constants that must stay in sync

The network name (`aws-bgp-poc`), namespace label key (`cluster-udn`), and
IPv4 subnet (`10.200.0.0/16`) are each declared independently in two places
that have to agree — `bgpRoutingNetworkName`/`bgpRoutingIPv4CIDR` in
`aws.go`, and `bgpCloudConnectorNamespaceLabelValue`/`bgpCloudConnectorIPv4CIDR`
in `kubevirt.go` (which can't import the `infraprovider` package, and
`aws.go` can't import the vendored `test/e2e` package, so there's no single
shared Go constant to use instead). Update both if you change either.

### Known limitation: cross-AZ path selection is not ECMP

With workers in two AZs, both correctly advertise the shared Layer2 UDN
prefix (`10.200.0.0/16`) via BGP — confirmed on both sides with `vtysh -c
"show bgp ipv4 unicast"` — but the AWS VPC route table installs only a
**single** next-hop ENI for that prefix, not ECMP across both. If the VM
scheduler places the VM on the AZ/node that currently *isn't* the one AWS
picked, inbound (north/south ingress) traffic arrives at the wrong node and
was observed to loop (`ping`/`iperf3` fail with ICMP "Time to live
exceeded" from that node, even at `ttl 255`) instead of being forwarded
across to the right node via OVN's Layer2 overlay. East/west (pod↔VM)
traffic between the same two nodes works correctly, so this looks specific
to externally-BGP-routed ingress combined with Layer2 UDN's
every-node-advertises-the-same-shared-prefix model, not a general
cross-node connectivity problem.

This is **not** introduced by the `bgp-cloud-connector` integration — both
modes generate functionally equivalent per-AZ `FRRConfiguration`s and the
same AWS route-table behavior applies either way. It was reproduced 3 of 4
runs (VM scheduled onto the non-preferred AZ each time); the one run where
placement happened to match passed cleanly end to end, including live
migration. Diagnosing/fixing the underlying AWS/OVN-Kubernetes interaction
is tracked as follow-up work; for now, a quick way to confirm or route
around it mid-run:

```bash
# Find which node AWS currently prefers for the UDN prefix vs where the VM is
oc get vmi -n <test-namespace> -o jsonpath='{.items[0].status.nodeName}'
aws ec2 describe-route-tables --route-table-ids <private-rtb-id> \
  --query 'RouteTables[0].Routes[?DestinationCidrBlock==`10.200.0.0/16`]'

# If they don't match, temporarily drop the non-matching AZ's node from
# BGP to force AWS onto the path that does (operator removes/recreates the
# peer automatically); restore once done
oc label node <non-matching-az-node> bgp_router-
oc label node <non-matching-az-node> bgp_router=true   # restore after
```

## Key design decisions

### Rootful podman (sudo)
Rootless podman uses slirp4netns networking which silently drops strongSwan's
IKE UDP packets. Rootful podman uses a real Linux bridge with kernel NAT. All
containers (FRR + iperf) run via `sudoRunner`.

### bgpnet on 172.29.0.0/24
The OCP service network is 172.30.0.0/16. Using 172.30.x.x for bgpnet caused
OVN on the worker nodes to intercept return traffic. 172.29.0.0/24 avoids this.

### AWS Security Group for bgpnet return traffic
AWS Security Groups do not track connections with non-ENI source IPs as
stateful. When the VM sends a TCP SYN with source = UDN IP (not the ENI's
own IP), the return SYN-ACK from bgpnet is dropped even though the outbound
SYN was allowed. Fix: explicit ingress rule allowing all TCP from bgpnet CIDR.

### TGW static route 10.0.0.0/8
UDN subnets are randomly allocated in 10.0.0.0/8 (by `randomCUDNSubnets()`)
but outside the VPC's 10.0.0.0/16. A static TGW route for 10.0.0.0/8 → VPC
attachment ensures return traffic reaches the VPC. The more-specific VPC local
route (10.0.0.0/16) takes priority for intra-VPC traffic.

### Per-AZ Route Server endpoints (no eBGP multihop)
Route Server endpoints are created in each worker's own subnet (one per AZ),
so workers peer with a directly-connected RS endpoint. This eliminates the
need for `ebgpMultiHop: true` in the FRRConfiguration and aligns with the
rosa-bgp-operator's per-AZ model. Each AZ gets its own FRRConfiguration CR
with a `topology.kubernetes.io/zone` node selector.

### Route Server propagation
Route Server learns UDN routes via eBGP from workers but does NOT automatically
install them in VPC route tables. `enable-route-server-propagation` must be
called for each private route table after RS-VPC association.

### No GRE on workers
The architecture diagram shows GRE/TGW Connect between TGW and on-prem only.
Workers do native eBGP with Route Server. GRE tunnels on workers were removed.

## Cleanup

AWS infrastructure is automatically cleaned up when the test finishes
(unless `AWS_KEEP_INFRA=1`). To force cleanup:

```bash
# Delete local containers
sudo podman rm -f frr
sudo podman network rm bgpnet

# AWS resources are tagged with the cluster infra ID and cleaned up
# by the reaper or by deleteAWSInfra() on test completion.
# Manual cleanup: delete TGW, VPN, Route Server, CGW via AWS console.
```

If `AWS_BGP_CLOUD_CONNECTOR=1` was used, delete the operator's CRs **before**
tearing down the raw AWS infra above — `BGPCloudConfiguration`'s finalizer
removes the Route Server peers it created (and all per-AZ
`FRRConfiguration`s) when it's deleted, so doing this first avoids deleting
the Route Server out from under still-registered peers. `BGPRouting`'s own
finalizer blocks `BGPCloudConfiguration`'s, so delete it first:

```bash
oc delete bgprouting aws-bgp-poc --wait
oc delete bgpcloudconfiguration cluster --wait
```

The Network operator patch (`additionalRoutingCapabilities: FRR`,
`routeAdvertisements: Enabled`) is intentionally **not** reverted by either
deletion — disabling FRR cluster-wide could disrupt other consumers. Node
labels (`bgp_router=true`) and `SourceDestCheck=false` on the worker ENIs
are likewise left in place; both are harmless to leave and the `bgp_router`
labels are reused as-is on the next run.
