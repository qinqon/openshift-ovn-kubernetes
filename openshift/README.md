# AWS BGP Infraprovider PoC — OCPSTRAT-3267

End-to-end test infrastructure for validating BGP routing with OpenShift
Virtualization on AWS. Tests VM egress source-IP preservation across live
migration using Route Server, Transit Gateway, and IPsec VPN.

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
  - FRR-k8s deployed (`additionalRoutingCapabilities: FRR`)
  - Route advertisements enabled (`routeAdvertisements: Enabled`)
  - Shared gateway mode (default, no `routingViaHost`)
  - At least 2 worker nodes
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
| `test/infraprovider/aws.go` | AWS provider: FRR container, strongSwan, BGP config |
| `test/infraprovider/aws_cluster.go` | Cluster info discovery (VPC, workers, subnets, SGs) |
| `test/infraprovider/aws_infra.go` | AWS resource lifecycle (Route Server, TGW, VPN, SG rules) |
| `test/infraprovider/aws_vpn.go` | VPN config parsing, swanctl template, xfrm interfaces |
| `test/infraprovider/openshift.go` | Provider interface (modified to support AWS fallback) |

## Upstream test modifications

The `test/e2e/kubevirt.go` file has minimal modifications to support
single-stack IPv4 clusters (AWS IPI clusters are single-stack by default):

- `networkDataForCluster()` — returns IPv4-only cloud-init network data when
  `!isIPv6Supported`, preventing NetworkManager from tearing down the connection
  when DHCPv6 fails.
- `externalContainerIPs` — gates IPv4/IPv6 append on `isIPv4Supported` /
  `isIPv6Supported`, fixing ICMP ping to IPv6 addresses on single-stack clusters.

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
