# Traefik as Reverse Proxy / Load Balancer

**What it is.** Traefik sits in front of the cloud-server and portal containers, terminating TLS and routing requests by `Host()` rule to the right backend service. It discovers backends automatically via the Docker provider — it watches the Docker socket and reads routing rules off container labels (`traefik.http.routers.*`), instead of a static config file that has to be hand-edited every time a container is added or scaled.

**Why we chose it (the tradeoff/alternative).** The alternative is nginx with a static `upstream` block listing each backend IP — that breaks the moment you `docker compose up --scale cloud-server=2`, because nginx doesn't know about the new container. Traefik's Docker provider re-reads the container list continuously, so scaling a service is enough; no proxy config change needed. The cost is an extra moving part (Traefik has to be trusted with the Docker socket) and a learning curve on label-based config vs a single nginx.conf.

**How it round-robins.** When two containers share the same `traefik.http.services.<name>.loadbalancer` label, Traefik's Docker provider sees both as backends for one service and load-balances between them — default is round robin. Each container gets its own internal network IP via the Docker bridge (`automail` network in [docker-compose.yml](../../docker-compose.yml)); Traefik forwards to whichever container's IP comes up next in rotation.

**The honest caveat.** Mounting `/var/run/docker.sock` into Traefik gives it the ability to introspect (and in principle, control) every container on the host — a real privilege-escalation surface if Traefik itself were compromised. Production deployments mitigate this with a `docker-socket-proxy` sidecar that exposes a read-only, scoped subset of the Docker API instead of the raw socket. Also: `curl https://api.automail.local/healthz` still needs `-k`/`--insecure`, but not because the edge cert is unwired — it now has its own dedicated self-signed cert (`infra/certs/gen-edge-certs.sh`, loaded as `tls.stores.default.defaultCertificate` in [infra/traefik/dynamic.yml](../../infra/traefik/dynamic.yml)), deliberately kept a *separate* trust domain from the internal mTLS CA (`infra/certs/gen.sh`) that secures the cloud↔printer hop. The two are never cross-wired by design, so `-k` is a permanent fact of this deployment (no real edge CA in front), not a "Phase 1" gap waiting to close.

---

## See also
- [infra/traefik/dynamic.yml](../../infra/traefik/dynamic.yml) — edge TLS store / default certificate
- [infra/certs/gen-edge-certs.sh](../../infra/certs/gen-edge-certs.sh) — edge cert generation (separate trust domain from mTLS)
- [infra/certs/gen.sh](../../infra/certs/gen.sh) — the internal mTLS CA, intentionally not the same cert
- [docker-compose.yml](../../docker-compose.yml) — Traefik service, Docker-provider labels, `automail` network
