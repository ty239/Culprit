# Broken pods: how to prove each failure

These are the commands to confirm each pod in `broken-pods.yaml` is broken, and to find the real reason why.
Every command was run against the `kind-culprit` cluster, and the outputs below are what it printed.

Run them from PowerShell, in any folder.

| Pod | What you see | Real cause | Best command to find the cause |
|---|---|---|---|
| `user-api` | `ErrImagePull` / `ImagePullBackOff` | Image `my-python-app:1.0` isn't on the cluster | `kubectl describe pod user-api` |
| `crash-api` | `CrashLoopBackOff` | `uvicorn` isn't installed in the image | `kubectl logs crash-api --previous` |
| `secret-api` | `CreateContainerConfigError` | Secret `db-credentials` doesn't exist | `kubectl get secret db-credentials` |
| `memory-api` | `OOMKilled` / `CrashLoopBackOff` | Uses more memory than its 64Mi limit | `kubectl get pod memory-api -o jsonpath=...` (see below) |
| `pending-api` | `Pending` | Asks for 100 CPUs, the node has 16 | `kubectl describe pod pending-api` |
| `storage-api` | `Pending` | Its PVC asks for StorageClass `fast-ssd`, which doesn't exist | `kubectl describe pvc storage-api-data` |
| `full-api` | `Pending` | Asks for 15.5 CPUs; the node has 16, but about 1 is already requested | `kubectl describe node culprit-control-plane` |
| `selector-api` | `Pending` | Wants a node labelled `disk=ssd`, no node has it | `kubectl get nodes --show-labels` |
| `probe-api` | `Running`, restarts keep going up | Its liveness probe asks for `/healthz`, which returns 404 | `kubectl describe pod probe-api` |
| `evicted-api` | `Error` (evicted) | Wrote 50MB to local disk, its limit is 10Mi | `kubectl get pod evicted-api -o jsonpath=...` (see below) |
| `init-api` | `Init:CrashLoopBackOff` | Its init container `migrate` always fails | `kubectl logs init-api -c migrate` |

`kubectl culprit POD` finds each of these by itself. The commands below are how to check its answer
by hand.

> **Events disappear after about an hour.** `kubectl describe` and `kubectl get events` only show
> the reason while the failure is fresh. For older failures, use the commands below that read the
> pod's stored status, or delete the pod and apply the file again to see fresh events.

---

## 1. `user-api`: image can't be downloaded

**Step 1: see the symptom**
```powershell
kubectl get pod user-api
```
```
NAME       READY   STATUS         RESTARTS   AGE
user-api   0/1     ErrImagePull   0          3d2h
```
It flips between `ErrImagePull` (a download just failed) and `ImagePullBackOff` (waiting before the next try). It's the same failure.

**Step 2: see which image it wants**
```powershell
kubectl get pod user-api -o jsonpath='{.spec.containers[0].image}'
```
```
my-python-app:1.0
```

**Step 3: see the error** (only while it's fresh)
```powershell
kubectl describe pod user-api
```
Look at **Events** at the bottom:
```
Failed to pull image "my-python-app:1.0": ... pull access denied, repository does not exist or may require authorization
```

**Step 4: prove the root cause.** The image isn't on the cluster's node:
```powershell
docker exec culprit-control-plane crictl images | Select-String my-python-app
```
No output means it's not there.

**Root cause:** `my-python-app:1.0` was built on the PC but never loaded into the cluster.

---

## 2. `crash-api`: app starts, then crashes

**Step 1: see the symptom**
```powershell
kubectl get pod crash-api
```
```
NAME        READY   STATUS             RESTARTS        AGE
crash-api   0/1     CrashLoopBackOff   134 (46s ago)   2d1h
```
The `RESTARTS` number keeps going up.

**Step 2: see how it ended last time**
```powershell
kubectl get pod crash-api -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason} {.status.containerStatuses[0].lastState.terminated.exitCode}'
```
```
Error 1
```
Exit code `1` means the app itself stopped with an error.

**Step 3: read the app's error message**
```powershell
kubectl logs crash-api --previous
```
```
/usr/local/bin/python: No module named uvicorn
```
`--previous` shows the logs from the last crash. Without it, you may get nothing, because the new attempt hasn't printed anything yet.

**Root cause:** the image `python:3.12-slim` has no `uvicorn`, but the command needs it.

---

## 3. `secret-api`: blocked before starting

**Step 1: see the symptom**
```powershell
kubectl get pod secret-api
```
```
NAME         READY   STATUS                       RESTARTS   AGE
secret-api   0/1     CreateContainerConfigError   0          25h
```
`RESTARTS` stays at `0` because the container never started.

**Step 2: see the error.** This is stored on the pod, so it doesn't expire:
```powershell
kubectl get pod secret-api -o jsonpath='{.status.containerStatuses[0].state.waiting.message}'
```
```
secret "db-credentials" not found
```

**Step 3: prove the root cause.** The Secret really doesn't exist:
```powershell
kubectl get secret db-credentials
```
```
Error from server (NotFound): secrets "db-credentials" not found
```

**Root cause:** the pod needs `password` from the Secret `db-credentials`, and that Secret was never created.

---

## 4. `memory-api`: killed for using too much memory

**Step 1: see the symptom**
```powershell
kubectl get pod memory-api
```
```
NAME         READY   STATUS             RESTARTS         AGE
memory-api   0/1     CrashLoopBackOff   27 (3m29s ago)   24h
```
It flips between `OOMKilled` and `CrashLoopBackOff`, and can briefly show `Running`.

**Step 2: see how it ended last time.** This is the key command:
```powershell
kubectl get pod memory-api -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason} {.status.containerStatuses[0].lastState.terminated.exitCode}'
```
```
OOMKilled 137
```
`OOMKilled` means "out of memory, killed". `137` is the code for a program that was force-stopped.

**Step 3: see its memory limit**
```powershell
kubectl get pod memory-api -o jsonpath='{.spec.containers[0].resources.limits.memory}'
```
```
64Mi
```

**Step 4: check the logs.** They're empty, which is a clue in itself:
```powershell
kubectl logs memory-api --previous
```
It prints nothing, because the app is killed before it can print anything.

**Root cause:** the app tries to use about 200MB, but its limit is 64Mi.

---

## 5. `pending-api`: can't be placed on a node

**Step 1: see the symptom**
```powershell
kubectl get pod pending-api -o wide
```
```
NAME          READY   STATUS    RESTARTS   AGE   IP       NODE     ...
pending-api   0/1     Pending   0          24h   <none>   <none>   ...
```
`NODE` is `<none>`, so it never got a node.

**Step 2: see the error.** This is stored on the pod, so it doesn't expire:
```powershell
kubectl get pod pending-api -o jsonpath='{.status.conditions[0].message}'
```
```
0/1 nodes are available: 1 Insufficient cpu. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.
```

**Step 3: prove the root cause.** Compare what it asks for with what the node has:
```powershell
kubectl get pod pending-api -o jsonpath='{.spec.containers[0].resources.requests.cpu}'
kubectl get node culprit-control-plane -o jsonpath='{.status.allocatable.cpu}'
```
```
100
16
```

**Root cause:** it asks for 100 CPUs, and the only node has 16.

---

## 6. `storage-api`: its disk can't be created

**Step 1: see the symptom**
```powershell
kubectl get pod storage-api
```
```
NAME          READY   STATUS    RESTARTS   AGE
storage-api   0/1     Pending   0          22h
```

**Step 2: see the error.** This is stored on the pod, so it doesn't expire:
```powershell
kubectl get pod storage-api -o jsonpath='{.status.conditions[0].message}'
```
```
0/1 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.
```
The pod is waiting for a disk, so follow the chain to its PVC.

**Step 3: look at the PVC**
```powershell
kubectl get pvc storage-api-data
```
```
NAME               STATUS    VOLUME   CAPACITY   ACCESS MODES   STORAGECLASS   AGE
storage-api-data   Pending                                      fast-ssd       22h
```
It's `Pending` too, and it asks for the StorageClass `fast-ssd`.

**Step 4: prove the root cause.** That StorageClass doesn't exist:
```powershell
kubectl get storageclass fast-ssd
```
```
Error from server (NotFound): storageclasses.storage.k8s.io "fast-ssd" not found
```
While it's fresh, `kubectl describe pvc storage-api-data` says the same under **Events**:
`storageclass.storage.k8s.io "fast-ssd" not found`.

**Root cause:** the PVC asks for StorageClass `fast-ssd`, and the cluster only has `standard`.

---

## 7. `full-api`: the node is big enough, but not empty

**Step 1: see the symptom and the error.** Same as `pending-api`:
```powershell
kubectl get pod full-api -o jsonpath='{.status.conditions[0].message}'
```
```
0/1 nodes are available: 1 Insufficient cpu. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.
```

**Step 2: compare what it asks for with what the node has**
```powershell
kubectl get pod full-api -o jsonpath='{.spec.containers[0].resources.requests.cpu}'
kubectl get node culprit-control-plane -o jsonpath='{.status.allocatable.cpu}'
```
```
15500m
16
```
15.5 is less than 16, so the node isn't too small. Something else is using the space.

**Step 3: prove the root cause.** See what the other pods have already requested:
```powershell
kubectl describe node culprit-control-plane
```
Look at **Allocated resources**:
```
  Resource           Requests    Limits
  cpu                950m (5%)   100m (0%)
```
16 minus 0.95 leaves 15.05 CPUs, and the pod asks for 15.5.

**Root cause:** the pods already on the node have requested 950m CPU, which leaves less than the 15.5 CPUs this pod asks for.

---

## 8. `selector-api`: asks for a label no node has

**Step 1: see the error.** Stored on the pod:
```powershell
kubectl get pod selector-api -o jsonpath='{.status.conditions[0].message}'
```
```
0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.
```

**Step 2: see what the pod asks for**
```powershell
kubectl get pod selector-api -o jsonpath='{.spec.nodeSelector}'
```
```
{"disk":"ssd"}
```

**Step 3: prove the root cause.** Show that label for every node:
```powershell
kubectl get nodes -L disk
```
```
NAME                    STATUS   ROLES           AGE   VERSION   DISK
culprit-control-plane   Ready    control-plane   14d   v1.31.0
```
The `DISK` column is empty: the node doesn't have the label.

**Root cause:** the pod only runs on nodes labelled `disk=ssd`, and no node is.

---

## 9. `probe-api`: a healthy app that keeps getting killed

**Step 1: see the symptom**
```powershell
kubectl get pod probe-api
```
```
NAME        READY   STATUS             RESTARTS      AGE
probe-api   0/1     CrashLoopBackOff   6 (71s ago)   4m51s
```
It flips between `Running` (even `1/1`) and `CrashLoopBackOff`, and `RESTARTS` keeps going up.

**Step 2: see how it ended last time**
```powershell
kubectl get pod probe-api -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason} {.status.containerStatuses[0].lastState.terminated.exitCode}'
```
```
Error 137
```
`137` means it was force-stopped from outside. It isn't `OOMKilled`, so it wasn't memory.

**Step 3: check the logs.** The app is working. The only "errors" are requests for a page it doesn't have:
```powershell
kubectl logs probe-api
```
```
10.244.0.1 - - [14/Nov/2026 18:29:24] code 404, message File not found
10.244.0.1 - - [14/Nov/2026 18:29:24] "GET /healthz HTTP/1.1" 404 -
```

**Step 4: prove the root cause** (only while it's fresh)
```powershell
kubectl describe pod probe-api
```
Look at **Events**:
```
Warning  Unhealthy  Liveness probe failed: HTTP probe failed with statuscode: 404
Normal   Killing    Container python-api failed liveness probe, will be restarted
```

**Root cause:** the liveness probe asks for `/healthz`, the app answers 404, so Kubernetes decides the app is dead and restarts it.

---

## 10. `evicted-api`: thrown out for filling its disk

**Step 1: see the symptom**
```powershell
kubectl get pod evicted-api
```
```
NAME          READY   STATUS   RESTARTS   AGE
evicted-api   0/1     Error    0          4m51s
```
`RESTARTS` stays at `0`: an evicted pod is not restarted.

**Step 2: see why it stopped.** This is stored on the pod, so it doesn't expire:
```powershell
kubectl get pod evicted-api -o jsonpath='{.status.phase} {.status.reason}: {.status.message}'
```
```
Failed Evicted: Pod ephemeral local storage usage exceeds the total limit of containers 10Mi.
```

**Step 3: see its disk limit**
```powershell
kubectl get pod evicted-api -o jsonpath='{.spec.containers[0].resources.limits.ephemeral-storage}'
```
```
10Mi
```

**Root cause:** the app writes a 50MB file to its own disk, but it's only allowed 10Mi.

---

## 11. `init-api`: stuck before the app even starts

**Step 1: see the symptom**
```powershell
kubectl get pod init-api
```
```
NAME       READY   STATUS                  RESTARTS       AGE
init-api   0/1     Init:CrashLoopBackOff   5 (2m7s ago)   4m51s
```
`Init:` means the problem is in an init container, not the app.

**Step 2: see how the init container ended**
```powershell
kubectl get pod init-api -o jsonpath='{.status.initContainerStatuses[0].lastState.terminated.reason} {.status.initContainerStatuses[0].lastState.terminated.exitCode}'
```
```
Error 1
```

**Step 3: read its error message.** Name the init container with `-c`:
```powershell
kubectl logs init-api -c migrate
```
```
migration failed: cannot reach database db:5432
```

**Root cause:** the init container `migrate` fails every time, so the app container is never started.

---

## Quick check for all of them at once

```powershell
kubectl get pods -l environment=development
```
Any pod that isn't `1/1` and `Running` is broken. Then use that pod's section above to find out why.
