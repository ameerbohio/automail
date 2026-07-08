// Client-side fetch helpers. Every call targets a same-origin Next.js API
// route under /api, which proxies to the cloud server (lib/proxy.ts). Going
// same-origin avoids CORS and keeps the cloud server's hostname server-side;
// the one exception is uploadBlob, which PUTs the ciphertext straight to the
// pre-signed MinIO URL (plans/06-sender-portal.md, plans/08-*).

export interface Recipient {
  recipient_id: string;
  display_name: string;
  building_address: string;
}

export interface UploadURL {
  upload_url: string;
  blob_ref: string;
  expires_in: number;
}

export interface CreateJobResult {
  job_id: string;
  status: string;
  guest_token?: string;
}

// jsonOrThrow surfaces the cloud server's own error message (its {error, code}
// envelope) so failures are legible in the UI instead of a bare status code.
async function jsonOrThrow<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let msg = `request failed (${res.status})`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body?.error) msg = body.error;
    } catch {
      // non-JSON error body -- keep the status-code message
    }
    throw new Error(msg);
  }
  return (await res.json()) as T;
}

export async function searchRecipients(q: string): Promise<Recipient[]> {
  const res = await fetch(`/api/recipients?q=${encodeURIComponent(q)}`);
  return jsonOrThrow<Recipient[]>(res);
}

export async function getRecipientPublicKey(id: string): Promise<string> {
  const res = await fetch(
    `/api/recipients/${encodeURIComponent(id)}/public-key`,
  );
  const body = await jsonOrThrow<{
    recipient_id: string;
    public_key_pem: string;
  }>(res);
  return body.public_key_pem;
}

export async function requestUploadURL(
  recipientId: string,
  filename: string,
): Promise<UploadURL> {
  const res = await fetch("/api/jobs/upload-url", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ recipient_id: recipientId, filename }),
  });
  return jsonOrThrow<UploadURL>(res);
}

// uploadBlob PUTs the encrypted blob directly to MinIO's pre-signed URL. The
// cloud server never receives the ciphertext -- it only ever holds the
// blob_ref pointer (plans/08-presigned-urls-direct-upload).
export async function uploadBlob(
  uploadURL: string,
  ciphertext: ArrayBuffer,
): Promise<void> {
  const res = await fetch(uploadURL, {
    method: "PUT",
    headers: { "Content-Type": "application/octet-stream" },
    body: ciphertext,
  });
  if (!res.ok) {
    throw new Error(`blob upload failed (${res.status})`);
  }
}

export async function createJob(input: {
  encrypted_key: string;
  blob_ref: string;
  recipient_id: string;
  page_count: number;
}): Promise<CreateJobResult> {
  const res = await fetch("/api/jobs", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  return jsonOrThrow<CreateJobResult>(res);
}
