# Keeper on Kubernetes

A Helm chart (`deploy/helm/keeper`) and a local kind setup that makes the
camera endpoint and the console reachable from the LAN through the Mac's IP.

## Quick start (kind on a Mac)

```bash
kind create cluster --name kind --config deploy/kind/cluster.yaml   # once; port mappings need a fresh cluster
deploy/kind-up.sh                                                     # build, load, secrets, install
```

`kind-up.sh` builds every image as `keeper/<app>:dev-<git sha>`, loads them
into the kind node (no registry), writes the secrets from `.env`, creates a
self-signed certificate on first run and installs the chart into the
`keeper` namespace. Extra Helm flags pass through, e.g.
`deploy/kind-up.sh --set lightroomMock.enabled=true` to test without Adobe.

| What | Where |
|---|---|
| Console | `http://<mac-ip>:8090` and `https://localhost:8443` (self-signed; the Adobe OAuth callback) |
| Camera | FTPES `<mac-ip>:2121`, user `camera`, password `CAMERA_FTP_PASSWORD` from `.env` |
| FTP passive ports | `30100–30109` |

macOS may ask once whether to allow incoming connections for `gvproxy`
(Podman) or Docker: allow it, or the camera cannot connect.

## Camera settings (Sony)

FTP transfer → server settings:

- Host: the Mac's LAN IP, port `2121`
- Secure protocol: **On** (explicit FTPS / FTPES)
- Root certificate error: **Ignore** (the certificate is self-signed)
- Passive mode: **On**
- User `camera`, password from `.env`, destination directory `/`

Test from the Mac without a camera:

```bash
curl --ssl-reqd -k --ftp-pasv -T test-pics/DSC07625.ARW -u camera:"$CAMERA_FTP_PASSWORD" ftp://<mac-ip>:2121/
```

## How the chart is put together

- One Deployment per Keeper service from `values.apps`, all running as uid
  1000, read-only spool mounts where a service only reads.
- NATS (JetStream), Postgres and Elasticsearch are small StatefulSets in the
  chart itself, so it installs offline. `search.enabled=false` drops
  Elasticsearch and the indexer.
- The collector is the only holder of the Dash0 token; inside Kubernetes it
  adds pod, namespace and deployment to every span, metric and log
  (`k8sattributes`).
- SFTPGo serves FTPES only (explicit TLS required). Its passive ports keep the
  same number on host, node and pod, because SFTPGo advertises them to the
  camera; `deploy/kind/cluster.yaml` and `values.sftpgo.passivePorts` must
  agree.
- Secrets are referenced, never templated: `keeper-secrets` (Dash0, FTP and
  Postgres passwords) and `keeper-tls` (certificate) are created by
  `kind-up.sh`.
- Camera-sim and the Lightroom mock are not deployed. The mock can be switched
  on for testing (`lightroomMock.enabled`).

## Limits

- **The spool is ReadWriteOnce.** That works on a single-node cluster such as
  kind because every pod runs on the same node. A multi-node cluster needs a
  ReadWriteMany storage class (NFS, EFS, CephFS) or object storage instead of
  the shared folder (the shortcut named in ADR-3).
- One release per namespace: services keep plain names (`ingest`, `nats`, …)
  so in-cluster URLs match docker compose.
- `files/sftpgo-*` and `files/collector.yaml` are copies of `sftpgo/` and
  `observability/collector.yaml` (Helm can only read files inside the chart);
  change both when changing one.
