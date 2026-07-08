// estimatePageCount is a best-effort in-browser page count used only as
// display metadata on the job (plans/09-api-contracts.md's page_count). It
// counts "/Type /Page" object markers in the raw PDF bytes: uncompressed PDFs
// expose them in plaintext, while PDFs using compressed object streams may
// hide them -- so we floor the result at 1. page_count is never trusted for
// routing, billing, or security, so an approximate value is acceptable for
// the prototype.
export function estimatePageCount(pdf: ArrayBuffer): number {
  const text = new TextDecoder("latin1").decode(pdf);
  // Match "/Type /Page" but not "/Type /Pages" (the page-tree root node).
  const matches = text.match(/\/Type\s*\/Page[^s]/g);
  const n = matches ? matches.length : 0;
  return n > 0 ? n : 1;
}
