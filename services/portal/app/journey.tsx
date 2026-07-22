"use client";

import {
  IconSealed,
  IconQueue,
  IconDispatch,
  IconPrinter,
  IconMailbox,
  IconEnvelope,
  Postmark,
} from "./icons";

// The status ladder a job climbs (plans/09-api-contracts.md). The server is the
// source of truth for the value; this array only decides how far along the
// route the document is drawn.
export const LADDER = [
  "submitted",
  "queued",
  "dispatching",
  "printing",
  "delivered",
] as const;

export function isTerminal(status: string): boolean {
  return status === "delivered" || status === "failed";
}

// Each stop pairs the wire status with a plain-language name and a one-line
// note about what actually happens to the bytes there -- the tracker doubles
// as an explanation of the security model.
const STOPS = [
  {
    id: "submitted",
    label: "Sealed",
    note: "Encrypted in your browser. The key was wrapped for one mailbox only.",
    Icon: IconSealed,
  },
  {
    id: "queued",
    label: "Queued",
    note: "Ciphertext parked in object storage. The server cannot read it.",
    Icon: IconQueue,
  },
  {
    id: "dispatching",
    label: "Dispatched",
    note: "Handed to the mailbox unit over its mutually-authenticated link.",
    Icon: IconDispatch,
  },
  {
    id: "printing",
    label: "Printing",
    note: "Decrypted in the printer's RAM only -- never written to a disk.",
    Icon: IconPrinter,
  },
  {
    id: "delivered",
    label: "Delivered",
    note: "Printed and in the box. The plaintext was wiped from memory.",
    Icon: IconMailbox,
  },
] as const;

export interface JobProgressProps {
  /** Latest status from the SSE stream, or null before the first frame. */
  current: string | null;
  /** status -> local time it arrived, for the per-stop timestamps. */
  times?: Record<string, string>;
}

/**
 * The postal route a document travels, rendered from live SSE status frames.
 *
 * Horizontal on a desktop, and the same DOM rotates to a vertical rail under
 * 560px (see globals.css) -- no second markup path, no JS breakpoint.
 */
export function JobProgress({ current, times = {} }: JobProgressProps) {
  const failed = current === "failed";
  const delivered = current === "delivered";

  // How far the job actually got: the furthest stop we have a timestamp for,
  // or the current status's position if this is the first frame we've seen.
  const seen = STOPS.reduce((max, s, i) => (times[s.id] ? i : max), -1);
  const named = current ? LADDER.indexOf(current as (typeof LADDER)[number]) : -1;
  const reached = Math.max(seen, failed ? -1 : named);

  // On failure, mark the stop it was heading for rather than the one it left.
  const failedAt = failed ? Math.min(reached + 1, STOPS.length - 1) : -1;

  const pct = reached <= 0 ? 0 : (reached / (STOPS.length - 1)) * 100;
  const activeNote = STOPS[Math.max(reached, 0)].note;

  return (
    <div
      className={`journey${delivered ? " is-done" : ""}${failed ? " is-failed" : ""}`}
      style={
        {
          // --progress positions the traveller along the rail; --p is the same
          // fraction unitless, for the rail fill's scale transform.
          "--progress": `${pct}%`,
          "--p": pct / 100,
          "--half-col": `${100 / (2 * STOPS.length)}%`,
        } as React.CSSProperties
      }
    >
      {/* journey-rows is the rail's positioning context. It must hug the stop
          list exactly -- if the rail were positioned against .journey it would
          also span the caption below and overshoot the last stop. */}
      <div className="journey-rows">
        <div className="journey-track" aria-hidden="true">
          <div className="journey-rail" />
          <div className="journey-rail-fill" />
          <div className="journey-traveler">
            <IconEnvelope size={13} />
          </div>
        </div>

        <ol className="journey-stops">
          {STOPS.map((s, i) => {
            const isFinal = i === STOPS.length - 1;
            let state = "";
            if (i === failedAt) state = " is-failed";
            else if (i < reached || (i === reached && (delivered || failed)))
              state = " is-done";
            else if (i === reached) state = " is-current";

            return (
              <li
                key={s.id}
                className={`stop${state}${isFinal ? " is-final" : ""}`}
                aria-current={i === reached && !failed ? "step" : undefined}
              >
                <span className="stop-dot">
                  <s.Icon size={19} />
                </span>
                <span className="stop-label">{s.label}</span>
                {times[s.id] && (
                  <span className="stop-time">{times[s.id]}</span>
                )}
              </li>
            );
          })}
        </ol>
      </div>

      {!failed && (
        <p className="journey-caption" key={activeNote}>
          {activeNote}
        </p>
      )}
    </div>
  );
}

/** The franking stamp struck over a job that made it all the way. */
export function DeliveredStamp({ at }: { at?: string }) {
  return (
    <div className="postmark">
      <Postmark
        id="pm-delivered"
        center="DELIVERED"
        // The delivery time takes the date position on a real datestamp.
        bottom={at ?? "ZERO-KNOWLEDGE"}
      />
    </div>
  );
}
