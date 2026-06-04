#!/bin/bash
# Run the AWS BGP e2e test for OCPSTRAT-3267
#
# Prerequisites:
#   - KUBECONFIG pointing to an AWS OpenShift cluster with:
#     - FRR-k8s deployed (additionalRoutingCapabilities: FRR)
#     - routeAdvertisements: Enabled
#     - CNV installed
#   - AWS CLI configured (aws sts get-caller-identity works)
#   - podman available locally
#
# Usage:
#   ./run-aws-bgp-test.sh
#
# Environment variables:
#   KUBECONFIG          - path to kubeconfig (default: ../kubeconfig)
#   GINKGO_FOCUS        - Ginkgo focus regex (default: routed L2 primary UDN live migration spec)
#   AWS_SKIP_INFRA_SETUP - set to skip AWS infra creation (reuse existing)
#   AWS_ONPREM_IP       - public IP for Customer Gateway (auto-detected if not set)
#   CONTAINER_RUNTIME   - podman or docker (default: podman)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export KUBECONFIG="${KUBECONFIG:-${SCRIPT_DIR}/../kubeconfig}"

echo "=== AWS BGP E2E Test ==="
echo "KUBECONFIG: ${KUBECONFIG}"
echo "Cluster: $(oc whoami --show-server 2>/dev/null || echo 'unknown')"
echo "AWS Identity: $(aws sts get-caller-identity --query 'Arn' --output text 2>/dev/null || echo 'unknown')"
echo ""

cd "${SCRIPT_DIR}"

exec go test -v -mod=vendor -count=1 -timeout 120m \
  -run "TestAWSBGP" \
  ./test/
