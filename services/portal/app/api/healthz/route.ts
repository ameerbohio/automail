export const dynamic = "force-dynamic";

// GET /api/healthz -- a probe target for the portal container.
//
// Unlike the cloud server's /healthz this checks no dependency on purpose: the
// portal is stateless and its only backend, the cloud API, has its own probe.
// What this answers is "is Next.js listening and serving routes yet", which is
// exactly what a readiness probe needs and what a container start alone does
// NOT prove -- Kubernetes marks a pod Ready the moment the process starts, so
// a rolling update with `maxUnavailable: 0` and no readiness probe still drops
// requests (plans/16-kubernetes.md §4.4).
//
// The alternative was probing `GET /`, which would render the SSR home page on
// every probe. This is the cheap version.
export function GET() {
  return Response.json({ status: "ok" });
}
