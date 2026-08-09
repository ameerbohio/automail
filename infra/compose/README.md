# Compose overlays

The real deployment is [`docker-compose.yml`](../../docker-compose.yml) in the repository root, and
it stays there — that is the file `docker compose up -d --build` picks up with no arguments, and the
one the deployment docs mean. Everything in this folder is an **override layered on top of it** for a
particular kind of run: a demo, a test profile, a load harness.

Each is invoked by a script, never by hand — the scripts set the environment variables and do the
ordering the overlay assumes. `make help` lists the entry points.

| Overlay | Entry point | What it changes |
|---|---|---|
| [`demo.yml`](demo.yml) | `make demo` | Adds a Cloudflare quick tunnel so the stack is reachable from a phone with no port forwarding or DNS. **Publishes your machine to the internet** — read `scripts/demo/up.sh`'s header first. |
| [`demo-print.yml`](demo-print.yml) | `make demo-print` | Layered on `demo.yml`. Turns *off* `DEV_MODE` so the printer makes the real `lp` call, and supplies a CUPS server for it to reach. |
| [`e2e.yml`](e2e.yml) | `make test-e2e` | Publishes the portal and MinIO on `localhost` so the Playwright browser reaches them without the Traefik TLS edge. |
| [`full.yml`](full.yml) | `make test-e2e-full`, `make chaos`, `make shutdown-check` | Runs **two** cloud nodes over one shared Redis/Postgres/MinIO — the profile that exercises the distributed seams. |
| [`deploy-smoke.yml`](deploy-smoke.yml) | `make deploy-smoke` | Deliberately changes almost nothing: the production profile, reached only through Traefik on the routed hostnames. |
| [`load.yml`](load.yml) | `make load` | One cloud node with the pprof profiler on, plus a k6 service inside the compose network (pre-signed URLs are signed for the internal `minio:9000` host). |
| [`k8s-printer.yml`](k8s-printer.yml) | `make k8s-e2e`, `make k8s-failure` | The odd one out — **not** an override. A standalone project running the printer outside the k3d cluster, dialing in. |

## Why the paths inside these files still start at the repository root

An overlay refers to build contexts and mounts as `./services/cloud`, `./infra/certs` and so on —
paths relative to the repo root, not to this folder. That keeps working after the move because
Compose resolves relative paths against the **project directory**, which it takes from the *first*
`-f` file:

```sh
docker compose -f docker-compose.yml -f infra/compose/full.yml up --build
#                ^ project directory comes from this one
```

Every script here puts the root `docker-compose.yml` first, so the project directory is the repo root
in all of them. `k8s-printer.yml` is the exception that proves the rule: it is never layered on the
base file, so it passes `--project-directory "$ROOT"` explicitly
([`scripts/k8s/lib-printer.sh`](../../scripts/k8s/lib-printer.sh)).

If you ever run one of these by hand without the base file first, pass `--project-directory .` from
the repo root or the build contexts will not resolve.
