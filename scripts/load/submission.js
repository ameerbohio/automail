// Job-submission throughput (testing-plan Part 8 / Goal T10). Ramps the guest
// submission arrival rate and records p95 latency + error rate for the POST
// /jobs call, so the knee (where latency climbs / errors appear) is visible.
//
// Runs INSIDE the compose network (see docker-compose.load.yml): the presigned
// upload URL is signed for the internal minio:9000 host, which this k6 container
// reaches directly. The full three-call guest flow is exercised per iteration
// (upload-url -> PUT ciphertext -> POST /jobs); only the POST is timed.
import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const BASE = __ENV.BASE_URL; // http://cloud-server:8080
const RECIPIENT = __ENV.RECIPIENT_ID; // seeded recipient uuid

// Valid base64 of ~513 bytes (RSA-4096-OAEP output size). The cloud stores
// encrypted_key verbatim and NEVER decrypts it (zero-knowledge), so synthetic
// bytes exercise the real submission path without any key material.
const ENC_KEY = 'A'.repeat(684);
const BLOB = 'x'.repeat(1024); // synthetic ciphertext blob

const submitDur = new Trend('submit_duration', true);
const submitFail = new Rate('submit_failed');

export const options = {
  scenarios: {
    submissions: {
      executor: 'ramping-arrival-rate',
      startRate: 5,
      timeUnit: '1s',
      preAllocatedVUs: 30,
      maxVUs: 120,
      stages: [
        { target: 10, duration: '15s' },
        { target: 30, duration: '20s' },
        { target: 50, duration: '20s' },
        { target: 50, duration: '10s' },
      ],
    },
  },
  // Guardrail thresholds -- a hard breach fails the k6 run (non-zero exit) so
  // `make load` stops. The richer baseline comparison is in check-baseline.py.
  thresholds: {
    submit_failed: ['rate<0.05'],
    submit_duration: ['p(95)<3000'],
  },
};

export default function () {
  const up = http.post(
    `${BASE}/jobs/upload-url`,
    JSON.stringify({ recipient_id: RECIPIENT, filename: 'load.pdf' }),
    { headers: { 'Content-Type': 'application/json' } }
  );
  if (!check(up, { 'upload-url 200': (r) => r.status === 200 })) {
    submitFail.add(1);
    return;
  }
  const { upload_url, blob_ref } = up.json();

  const put = http.put(upload_url, BLOB, {
    headers: { 'Content-Type': 'application/octet-stream' },
  });
  if (!check(put, { 'upload PUT 200': (r) => r.status === 200 })) {
    submitFail.add(1);
    return;
  }

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
  submitDur.add(res.timings.duration);
  const ok = check(res, { 'POST /jobs 202': (r) => r.status === 202 });
  submitFail.add(!ok);
}

export function handleSummary(data) {
  return { '/report/submission.json': JSON.stringify(data, null, 2) };
}
