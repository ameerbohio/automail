// SSE fan-out boundedness (testing-plan Part 8 / Goal T10). Opens many
// concurrent GET /jobs/:id/stream subscribers on ONE job and holds them, so
// run.sh can snapshot the cloud's goroutine/heap count via pprof at peak and
// after release. The scaling question this answers: the StreamJob handler opens
// one Redis subscription PER connection (handlers/jobs.go), so goroutines grow
// ~linearly with subscribers -- what matters is that they RETURN to baseline
// when the clients disconnect (no per-connection leak). run.sh asserts that.
//
// The job must stay non-terminal for the hold, or the server closes the stream
// early: run.sh stops the printer first so the setup() job sits in 'queued'.
import http from 'k6/http';

const BASE = __ENV.BASE_URL;
const RECIPIENT = __ENV.RECIPIENT_ID;
const SUBSCRIBERS = parseInt(__ENV.SUBSCRIBERS || '150', 10);
const HOLD = __ENV.HOLD_SECONDS || '25';

const ENC_KEY = 'A'.repeat(684);
const BLOB = 'x'.repeat(1024);

export const options = {
  scenarios: {
    fanout: {
      executor: 'per-vu-iterations',
      vus: SUBSCRIBERS,
      iterations: 1,
      maxDuration: '90s',
    },
  },
};

// setup() runs once, in-network: submit a single job whose stream every VU will
// hold open. With the printer stopped (run.sh does this before launching), the
// job stays 'queued' so the server keeps the stream open for the whole hold.
export function setup() {
  const up = http.post(
    `${BASE}/jobs/upload-url`,
    JSON.stringify({ recipient_id: RECIPIENT, filename: 'hold.pdf' }),
    { headers: { 'Content-Type': 'application/json' } }
  );
  const { upload_url, blob_ref } = up.json();
  http.put(upload_url, BLOB, {
    headers: { 'Content-Type': 'application/octet-stream' },
  });
  const res = http.post(
    `${BASE}/jobs`,
    JSON.stringify({
      encrypted_key: ENC_KEY,
      blob_ref,
      recipient_id: RECIPIENT,
      page_count: 1,
    }),
    { headers: { 'Content-Type': 'application/json' } }
  );
  const body = res.json();
  if (!body.job_id || !body.guest_token) {
    throw new Error(`hold-job submit failed: ${res.status} ${res.body}`);
  }
  return { url: `${BASE}/jobs/${body.job_id}/stream?token=${body.guest_token}` };
}

export default function (data) {
  // Hold the SSE connection for HOLD seconds. Each held GET keeps one server
  // goroutine + Redis subscription alive -- the concurrent-subscriber pressure
  // pprof measures. The GET blocks until the server closes (it won't, job is
  // queued) or this per-request timeout fires.
  http.get(data.url, {
    headers: { Accept: 'text/event-stream' },
    timeout: `${HOLD}s`,
  });
}
