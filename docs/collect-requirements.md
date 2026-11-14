# Collect: requirements

Collect is the first step of Culprit. It connects to a Kubernetes cluster, starts from one pod,
gathers every fact about it and the things it depends on, and saves them as one snapshot (JSON).
It does **not** work out the root cause. That's a separate step that reads the snapshot.

Language: Go, using `client-go`.

---

## Part 1: Connecting to the cluster

Collect finds the cluster the same way `kubectl` does, through the kubeconfig file.

| Requirement | Why |
|---|---|
| Load kubeconfig from `KUBECONFIG`, or `~/.kube/config` if that isn't set | Same place `kubectl` looks, so no extra setup |
| Accept `--context`, `--namespace` and `--kubeconfig` flags, defaulting to the current ones | To choose which cluster and namespace to look at |
| Check the connection first, with a timeout | Fail fast with a clear message if the cluster is down, instead of hanging |
| Read-only: never create, change or delete anything | It runs against clusters that are already broken |

Use `k8s.io/cli-runtime` (`genericclioptions`) to get the standard flags for free and to run as a
`kubectl culprit` plugin.

### Permissions
Collect needs read access (`get` / `list`) to:

pods, `pods/log`, events, nodes, secrets, configmaps, persistentvolumeclaims,
persistentvolumes, storageclasses, serviceaccounts

For a pod that hasn't been placed on a node, it also lists the pods in **every** namespace, to add
up what is already requested on each node.

If a read is denied, record that in the snapshot and keep collecting.

---

## Part 2: Grabbing the information

Start from one pod, collect everything about it, then follow what it points to.

| What to grab | Fields | Pod that needs it |
|---|---|---|
| **Pod: its stored status** | `status.phase`; per container: `state.waiting.reason` / `message`, `lastState.terminated.reason` / `exitCode`; `status.conditions` | `secret-api`, `memory-api`, `pending-api` |
| **Logs, current and previous** | Last ~100 lines per container, including the previous container (`--previous`) | `crash-api` |
| **Events** | All events whose `involvedObject` is the pod | `user-api` (while fresh) |
| **Nodes** | Allocatable CPU and memory, conditions, and the images on the node (`status.images`) | `pending-api`, `user-api` |
| **What's already requested on each node** | Total CPU and memory requests of the pods on the node, and how many pods. Only when the pod has no node yet | `full-api` |
| **Secrets and ConfigMaps it uses** | Whether each exists, and its key **names only, never the values** | `secret-api` |
| **Storage it uses** | PVC → PV → StorageClass | The pending-pod chain in the README |
| **Service account** | Whether it exists | Pods blocked by a missing account |

Find Secrets and ConfigMaps from `env[].valueFrom`, `envFrom`, `volumes` and `imagePullSecrets`.

---

## Rules

1. **"Not found" is evidence, not an error.** If a Secret, ConfigMap, PVC or service account the pod
   uses doesn't exist, record it as missing. For `secret-api`, that *is* the answer.
2. **Get nodes even when the pod has no node.** A `Pending` pod has no `spec.nodeName`, but the
   node's capacity explains why. If the pod has no node, list all nodes.
3. **Never stop at the first failure.** If one part fails, keep what succeeded and record what failed.
4. **Expect logs to be missing.** A container that never started has no logs, and one killed for
   memory (`OOMKilled`) may have empty logs. That's normal, not an error.
5. **Events expire after about an hour.** Never rely on events alone. The pod's stored status
   (the first row of the table above) must always be collected.
6. **Never store Secret values.** Only names and key names.
7. **Collect, don't diagnose.** No root-cause logic in this step.

---

## Done when

For each test pod in `broken-pods.yaml`, the snapshot contains the evidence listed in
`broken-pods-evidence.md`:

| Pod | The snapshot must contain |
|---|---|
| `user-api` | Waiting reason `ErrImagePull` / `ImagePullBackOff`, the image name, and the node's image list without it |
| `crash-api` | Last termination `Error`, exit code `1`, and previous logs with `No module named uvicorn` |
| `secret-api` | Waiting reason `CreateContainerConfigError`, its message, and Secret `db-credentials` marked as missing |
| `memory-api` | Last termination `OOMKilled`, exit code `137`, and the `64Mi` memory limit |
| `pending-api` | Phase `Pending`, the `PodScheduled` condition message, the `100` CPU request, and the node's `16` allocatable CPUs |
| `storage-api` | Phase `Pending`, PVC `storage-api-data` with phase `Pending` and class `fast-ssd`, and StorageClass `fast-ssd` marked as missing |
| `full-api` | The `15500m` CPU request, the node's `16` allocatable CPUs, and the CPU already requested by the pods on the node |

Tests can run without a cluster by using `client-go`'s fake client (`k8s.io/client-go/kubernetes/fake`)
loaded with copies of these pods.
