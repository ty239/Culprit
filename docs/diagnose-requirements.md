# Diagnose: requirements

Diagnose is the second step of Culprit. It reads the snapshot that collect wrote and works out
why the pod is failing: it starts at the visible symptom and follows the `causedBy` chain to the
root cause.

It never talks to the cluster. Everything it says must come from the snapshot.

Language: Go, package `internal/diagnose`.

---

## Part 1: What it produces

A **diagnosis**: a list of findings, and the evidence behind them.

### Finding

| Field | Meaning |
|---|---|
| `id` | `f-1`, `f-2`, ... |
| `kind` | `symptom` (what you see first), `cause` (a step in between), or `rootCause` (where the chain ends) |
| `object` | The Kubernetes object it is about, e.g. `Pod default/storage-api`, `StorageClass fast-ssd` |
| `summary` | One plain sentence |
| `causedBy` | The `id` of the finding that caused this one. Empty on the last finding of a chain |
| `evidence` | The `ev-N` records that back it up |
| `fix` | Root causes only: what to change |

### Evidence

| Field | Meaning |
|---|---|
| `id` | `ev-1`, `ev-2`, ... |
| `source` | Where in the snapshot it came from, e.g. `pvcs[storage-api-data].phase` |
| `value` | What was found there, copied from the snapshot |

### Example: `storage-api`

```
f-1  symptom    Pod default/storage-api          can't be scheduled            causedBy f-2
f-2  cause      PersistentVolumeClaim ...-data   Pending, no disk bound to it  causedBy f-3
f-3  rootCause  StorageClass fast-ssd            doesn't exist
```

---

## Part 2: The rules

Each rule looks for one symptom, then follows it down.

| Symptom | How to follow the chain | Test pod |
|---|---|---|
| **Pod not scheduled** (`PodScheduled` is `False`) | 1. A PVC is missing, or `Pending` → its StorageClass is missing / there is no default class. 2. The scheduler says `Insufficient X` → compare the pod's request with the biggest node's allocatable. 3. The scheduler mentions a node selector, a taint, or a cordoned node → compare with the nodes' labels and taints | `storage-api`, `pending-api` |
| **Image can't be pulled** (`ErrImagePull`, `ImagePullBackOff`, `InvalidImageName`) | A missing `imagePullSecret` → that Secret. Otherwise the image itself: the pull error, and whether the image is on the node | `user-api` |
| **Container blocked before starting** (`CreateContainerConfigError`, or stuck in `ContainerCreating`) | A Secret or ConfigMap it needs is missing, or exists without the key it asks for | `secret-api` |
| **Container keeps crashing** (`CrashLoopBackOff`, or terminated with a non-zero exit code) | Last termination is `OOMKilled` → the memory limit. Otherwise → the exit code → the last line the app logged | `memory-api`, `crash-api` |
| **Container can't be started** (`RunContainerError`, `CreateContainerError`, `StartError`) | The runtime's message, e.g. a command that isn't in the image | |
| **Running but not ready** | Reported as a symptom, with the kubelet's `Unhealthy` event if there is one | |

One cause per symptom: when several things are wrong, report the first one that blocks the pod.
Fix it, run again, and the next one shows up.

---

## Rules

1. **Every finding needs evidence.** No finding without at least one `ev-N` that points into the
   snapshot.
2. **Don't guess.** If the chain can't be followed any further, stop and say the root cause wasn't
   found. A symptom with no root cause is an honest answer; a made-up one isn't.
3. **Don't rely on events.** They expire after about an hour. Use the pod's stored status first and
   add events as extra evidence when they are there.
4. **The pod's state flips.** `memory-api` shows `OOMKilled`, then `CrashLoopBackOff`, then
   `Running` for a moment. `user-api` flips between `ErrImagePull` and `ImagePullBackOff`. The
   answer must be the same whichever one the snapshot caught.
5. **Logs may be on either side.** After a crash, the error can be in the current log or the
   previous one, depending on when the snapshot was taken. Check both.
6. **A healthy pod is healthy.** Running and ready means no findings, even if it restarted once in
   the past.
7. **Say when the snapshot is incomplete.** If collect recorded failed reads (`errors`), list them:
   the cause may be in something that couldn't be read.
8. **Only read the snapshot.** No cluster calls, so a saved snapshot can be diagnosed later, or on
   another machine.

---

## Part 3: The command

| Command | What it prints |
|---|---|
| `kubectl culprit POD` | The chain, in plain text |
| `kubectl culprit POD -o json` | The diagnosis as JSON |
| `kubectl culprit POD -o snapshot` | The raw snapshot (collect only, no diagnosis) |
| `kubectl culprit --from snap.json` | Diagnose a saved snapshot, without a cluster |

---

## Done when

For each test pod, diagnosing a snapshot taken from the real `kind-culprit` cluster gives:

| Pod | Symptom | Root cause |
|---|---|---|
| `user-api` | Image can't be pulled | Image `my-python-app:1.0` isn't on the node and can't be pulled from a registry |
| `crash-api` | Container keeps crashing | The app fails with `No module named uvicorn` |
| `secret-api` | Blocked with `CreateContainerConfigError` | Secret `db-credentials` doesn't exist |
| `memory-api` | Container keeps crashing | Killed for memory: the `64Mi` limit is too low |
| `pending-api` | Pod can't be scheduled | Asks for `100` CPUs, the biggest node has `16` |
| `storage-api` | Pod can't be scheduled | PVC `storage-api-data` is `Pending` because StorageClass `fast-ssd` doesn't exist |
| `my-app` (healthy) | none | none |

Tests read those snapshots from `internal/diagnose/testdata/`, so they run without a cluster.

---

## Not yet

- Liveness probes that kill a healthy container
- Evicted pods and node pressure
- "The nodes are big enough, but other pods have used the space" (needs the other pods' requests,
  which collect doesn't gather yet)
- Init containers get the same rules as normal containers, but there is no test pod for them
