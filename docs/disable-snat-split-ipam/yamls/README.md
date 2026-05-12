# Split IPAM — Deployment YAMLs

This directory contains the Kubernetes manifests needed to deploy the
OVN-Kubernetes + HBN stack with split IPAM (separate pools for VTEP and
host interface).

---

## Variable substitution

Before applying, substitute the following placeholders in the relevant files:

| Placeholder                      | Description                                     | Files                                                       |
| -------------------------------- | ----------------------------------------------- | ----------------------------------------------------------- |
| `$TAG`                           | Image/chart/flavor version tag                  | `bfb.yaml`, `dpudeployment.yaml`, `dpuflavor-hbn-ovnk.yaml` |
| `$BFB_URL`                       | URL to the BFB image                            | `bfb.yaml`                                                  |
| `$TARGETCLUSTER_API_SERVER_HOST` | Host cluster API server hostname or IP          | `dpuserviceconfig_ovn.yaml`                                 |
| `$TARGETCLUSTER_API_SERVER_PORT` | Host cluster API server port (typically `6443`) | `dpuserviceconfig_ovn.yaml`                                 |
| `$POD_CIDR`                      | Pod network CIDR of the host cluster            | `dpuserviceconfig_ovn.yaml`                                 |
| `$SERVICE_CIDR`                  | Service network CIDR of the host cluster        | `dpuserviceconfig_ovn.yaml`                                 |
| `$TARGETCLUSTER_NODE_CIDR`       | Node CIDR of the host cluster                   | `dpuserviceconfig_ovn.yaml`                                 |
| `$HELM_REGISTRY_REPO_URL`        | Helm registry URL for the HBN chart             | `dpuservicetemplate_hbn.yaml`                               |
| `$HBN_NGC_IMAGE_URL`             | HBN container image repository URL              | `dpuservicetemplate_hbn.yaml`                               |
| `$OVN_KUBERNETES_REPO_URL`       | Helm registry URL for the OVN-Kubernetes chart  | `dpuservicetemplate_ovn.yaml`                               |
| `$OVN_KUBERNETES_CHART_TAG`      | OVN-Kubernetes chart version                    | `dpuservicetemplate_ovn.yaml`                               |

> **Note**: `worker1*` and `worker2*` hostname patterns in `dpuserviceconfig_hbn.yaml`
> under `perDPUValuesYAML` should be updated to match your actual worker node hostnames.

---

## Deployment order

Apply the manifests in the following order. Each step should be fully ready
before proceeding to the next.

### Step 1 — BFB and DPU Flavor

These define the firmware and OS configuration for the DPUs.

```bash
kubectl apply -f bfb.yaml
kubectl apply -f dpuflavor-hbn-ovnk.yaml
```

### Step 2 — IPAM pools

Define the IP address pools for the loopback, host interface, and VTEP networks.

```bash
kubectl apply -f hbn-loopback-ipam.yaml
kubectl apply -f hbn-ovn-ipam.yaml
```

### Step 3 — Credentials

Create the OVN credential request so the DPU can authenticate with the host cluster.

```bash
kubectl apply -f ovn-credentials.yaml
```

### Step 4 — Service interfaces

Define the DPU service interfaces for physical uplinks, the OVN patch port, and the VTEP bridge.

```bash
kubectl apply -f physical-ifaces.yaml
kubectl apply -f ovn-iface.yaml
kubectl apply -f vtep-iface.yaml
```

### Step 5 — Service templates

Define the Helm chart sources and base values for HBN and OVN-Kubernetes.

```bash
kubectl apply -f dpuservicetemplate_hbn.yaml
kubectl apply -f dpuservicetemplate_ovn.yaml
```

### Step 6 — Service configurations

Apply the runtime configuration for HBN (BGP, interfaces) and OVN-Kubernetes (IPAM, CIDRs).

```bash
kubectl apply -f dpuserviceconfig_hbn.yaml
kubectl apply -f dpuserviceconfig_ovn.yaml
```

### Step 7 — DPU Deployment

Create the DPUDeployment which ties everything together and triggers provisioning.

```bash
kubectl apply -f dpudeployment.yaml
```

---

## Post-deployment verification

After applying all manifests, verify the stack is healthy in this order:

```bash
# 1. Wait for all DPUs to be provisioned and ready
kubectl wait --for=condition=ready dpus -A --all --timeout=300s

# 2. Wait for tenant cluster nodes to be ready
kubectl --kubeconfig=<tenant-kubeconfig> wait --for=condition=ready nodes -A --all --timeout=300s

# 3. Wait for all pods on tenant cluster to be ready
kubectl --kubeconfig=<tenant-kubeconfig> wait --for=condition=ready pods -A --all --timeout=300s

# 4. Wait for OVN-Kubernetes DPU host pods to be ready
kubectl wait --for=condition=ready --namespace ovn-kubernetes \
  pods -l app.kubernetes.io/component=ovnkube-node-dpu-host --timeout=300s

# 5. Final check — all pods across all namespaces
kubectl wait --for=condition=Ready --all-namespaces pods --all --timeout=300s
```
