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

> This pod is still running on the cluster, but its YAML is currently **not** in `broken-pods.yaml`.

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

