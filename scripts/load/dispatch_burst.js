// Dispatch backlog burst (testing-plan Part 8 / Goal T10, Phase C). Submits a
// fixed burst of jobs while the printer is OFFLINE so every one of them is
// forced down the *queued* path onto the jobs:pending Redis Stream, instead of
// the immediate-dispatch path Phase A exercises.
//
// Why offline forces the queue: with no live printer socket, the owner node's
// PUBLISH mailbox:<id>:dispatch gets 0 receivers, so dispatch reverts the claim
// and enqueues the job (see dispatch/route.go attemptDispatch -> enqueue). That
// builds the backlog whose drain run.sh then times once the printer returns --
// the "consumer group keeps up / lag is bounded" property Part 8 asks for.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL;
const RECIPIENT = __ENV.RECIPIENT_ID;
const BURST = parseInt(__ENV.BURST || '60', 10);

const ENC_KEY = 'A'.repeat(684);
const BLOB = 'x'.repeat(1024);

export const options = {
  scenarios: {
    burst: {
      executor: 'shared-iterations',
      vus: 10,
      iterations: BURST,
      maxDuration: '60s',
    },
  },
};

export default function () {
  const up = http.post(
    `${BASE}/jobs/upload-url`,
    JSON.stringify({ recipient_id: RECIPIENT, filename: 'burst.pdf' }),
    { headers: { 'Content-Type': 'application/json' } }
  );
  if (!check(up, { 'upload-url 200': (r) => r.status === 200 })) return;
  const { upload_url, blob_ref } = up.json();

  const put = http.put(upload_url, BLOB, {
    headers: { 'Content-Type': 'application/octet-stream' },
  });
  if (!check(put, { 'upload PUT 200': (r) => r.status === 200 })) return;

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
  // With the printer offline the expected status is "queued" -- that is the
  // point of this phase. 202 either way; we only assert the submit succeeded.
  check(res, { 'POST /jobs 202': (r) => r.status === 202 });
}
