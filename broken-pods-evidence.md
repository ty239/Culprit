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

