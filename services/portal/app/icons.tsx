// Hand-drawn icon set. Everything the portal renders is authored here as
// inline SVG rather than pulled from an icon package: it keeps the bundle at
// zero extra dependencies, lets each glyph inherit `currentColor` (so light
// and dark mode need no second asset), and keeps the drawing style consistent
// -- 24x24 box, 1.5px strokes, round joins, one visual weight throughout.
//
// Every icon is decorative: callers put the meaning in text, so the SVGs are
// aria-hidden and never contribute to an accessible name.

import type { SVGProps } from "react";

type IconProps = SVGProps<SVGSVGElement> & { size?: number };

function Icon({ size = 20, children, ...rest }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      {children}
    </svg>
  );
}

/* ------------------------------------------------------------------ brand */

// The mark: a perforated stamp with an envelope cut out of it. The
// perforations are a round-cap dotted stroke straddling the stamp's edge --
// one <rect> instead of thirty-two notch circles.
export function Logo({ size = 26 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 64 64"
      aria-hidden="true"
      focusable="false"
    >
      <rect x="5" y="5" width="54" height="54" rx="7" fill="var(--stamp-blue)" />
      <rect
        x="5"
        y="5"
        width="54"
        height="54"
        rx="7"
        fill="none"
        stroke="var(--paper)"
        strokeWidth="5"
        strokeLinecap="round"
        strokeDasharray="0 7.6"
      />
      <g
        stroke="var(--paper-raised)"
        strokeWidth="3"
        strokeLinecap="round"
        strokeLinejoin="round"
        fill="none"
      >
        <rect x="17" y="23" width="30" height="21" rx="3" />
        <path d="m18.5 25 13.5 9.5L45.5 25" />
      </g>
      {/* Wax seal over the flap join -- the "sealed" half of the story. */}
      <circle cx="45" cy="42" r="6.5" fill="var(--stamp-red)" />
      <circle cx="45" cy="42" r="6.5" fill="none" stroke="var(--paper-raised)" strokeWidth="2" />
    </svg>
  );
}

/* --------------------------------------------------- journey stop glyphs */

// 1. Submitted -- a sealed envelope.
export function IconSealed(p: IconProps) {
  return (
    <Icon {...p}>
      <rect x="2.75" y="5.5" width="18.5" height="13" rx="2" />
      <path d="m3.4 6.8 8.6 5.9 8.6-5.9" />
      <circle cx="18.5" cy="16.5" r="3" fill="currentColor" stroke="none" />
    </Icon>
  );
}

// 2. Queued -- an inbox tray.
export function IconQueue(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M20.5 13.4v4.1a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2v-4.1" />
      <path d="M3.5 13.4h4l1.3 2.4h6.4l1.3-2.4h4" />
      <path d="m3.5 13.4 2.6-7.8a1.5 1.5 0 0 1 1.4-1h9a1.5 1.5 0 0 1 1.4 1l2.6 7.8" />
    </Icon>
  );
}

// 3. Dispatching -- handed to the mailbox unit over the printer link.
export function IconDispatch(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M21.4 3.1 2.9 9.9a.6.6 0 0 0 0 1.1l7.4 2.8 2.8 7.4a.6.6 0 0 0 1.1 0z" />
      <path d="M21.4 3.1 10.3 13.8" />
    </Icon>
  );
}

// 4. Printing.
export function IconPrinter(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M7 8.5v-4a1 1 0 0 1 1-1h8a1 1 0 0 1 1 1v4" />
      <path d="M7 17.5H5.5a2 2 0 0 1-2-2V11a2 2 0 0 1 2-2h13a2 2 0 0 1 2 2v4.5a2 2 0 0 1-2 2H17" />
      <path d="M7 14.5h10v5a1 1 0 0 1-1 1H8a1 1 0 0 1-1-1z" />
      <path d="M17.3 11.8h.01" />
    </Icon>
  );
}

// 5. Delivered -- a mailbox with the flag up.
export function IconMailbox(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M2.75 19v-5a4.5 4.5 0 0 1 4.5-4.5h9.25V19z" />
      <path d="M6 14.2h4" />
      <path d="M6.75 19v2.5" />
      <path d="M20 10.2V4.6" />
      <path d="M20 5.3h-3.6l1.1 1.5-1.1 1.5H20z" fill="currentColor" />
    </Icon>
  );
}

/* ----------------------------------------------------------- utility set */

export function IconEnvelope(p: IconProps) {
  return (
    <Icon {...p}>
      <rect x="2.75" y="5.5" width="18.5" height="13" rx="2" />
      <path d="m3.4 6.8 8.6 5.9 8.6-5.9" />
    </Icon>
  );
}

export function IconLock(p: IconProps) {
  return (
    <Icon {...p}>
      <rect x="4.5" y="10.25" width="15" height="9.75" rx="2" />
      <path d="M7.75 10.25V7.5a4.25 4.25 0 0 1 8.5 0v2.75" />
      <path d="M12 14v2.5" />
    </Icon>
  );
}

// Zero-knowledge: the server cannot see. A struck-through eye.
export function IconEyeOff(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M10.7 5.4A8.9 8.9 0 0 1 12 5.3c5 0 9 4.3 9 6.7 0 .9-.6 2-1.6 3.1" />
      <path d="M6.4 7.3C4.3 8.7 3 10.7 3 12c0 2.4 4 6.7 9 6.7 1.5 0 2.9-.4 4.1-1" />
      <path d="M9.9 9.9a3 3 0 0 0 4.2 4.2" />
      <path d="m3.6 3.6 16.8 16.8" />
    </Icon>
  );
}

export function IconCheck(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="m4.5 12.5 5 5 10-11" />
    </Icon>
  );
}

export function IconAlert(p: IconProps) {
  return (
    <Icon {...p}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7.75v5" />
      <path d="M12 16.2h.01" />
    </Icon>
  );
}

export function IconSearch(p: IconProps) {
  return (
    <Icon {...p}>
      <circle cx="10.75" cy="10.75" r="6.25" />
      <path d="m15.4 15.4 4.1 4.1" />
    </Icon>
  );
}

export function IconUpload(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M14 3.5H8a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V7.5z" />
      <path d="M13.75 3.5v4h4.25" />
      <path d="M12 16.5v-5.25" />
      <path d="m9.9 13.3 2.1-2 2.1 2" />
    </Icon>
  );
}

// A cloud node — the stateless server instances the job is routed through.
export function IconNode(p: IconProps) {
  return (
    <Icon {...p}>
      <rect x="3.25" y="4" width="17.5" height="6.25" rx="2" />
      <rect x="3.25" y="13.75" width="17.5" height="6.25" rx="2" />
      <path d="M7 7.1h.01" />
      <path d="M7 16.9h.01" />
    </Icon>
  );
}

export function IconArrowRight(p: IconProps) {
  return (
    <Icon {...p}>
      <path d="M4.5 12h15" />
      <path d="m13.5 6 6 6-6 6" />
    </Icon>
  );
}

/* ------------------------------------------------------------- postmark */

// A franking postmark, laid out the way a real datestamp is: origin arced
// over the top, date arced under the bottom, and the cancel word struck
// across the middle with a bar. Used full-strength over a delivered job and
// at ~5% opacity as the hero watermark. `id` namespaces the two textPath
// anchors so more than one postmark can coexist on a page.
export function Postmark({
  id = "pm",
  size = 132,
  top = "AUTOMAIL",
  bottom = "ZERO-KNOWLEDGE",
  center = "DELIVERED",
}: {
  id?: string;
  size?: number;
  top?: string;
  bottom?: string;
  center?: string;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 120 120"
      fill="none"
      stroke="currentColor"
      aria-hidden="true"
      focusable="false"
      style={{ fontFamily: "var(--font-sans)" }}
    >
      {/* Worn outer ring -- a long dash pattern reads as an inked stamp that
          didn't strike evenly, which is what stops it looking like a badge. */}
      <circle cx="60" cy="60" r="55" strokeWidth="2.5" strokeDasharray="26 4" />
      <circle cx="60" cy="60" r="45" strokeWidth="1" />

      {/* Text baselines, set at r=38 so the glyph tops stop just inside the
          r=45 ring. The top arc sweeps over the top (letters upright); the
          bottom one sweeps under, so its letters point at the centre the way a
          real datestamp reads.

          Keep both strings short. Arc length here is ~119 units per
          semicircle and a character costs ~6, so anything past ~18 characters
          wraps round the sides and collides with the struck word. */}
      <path id={`${id}-top`} d="M60 60m-38 0a38 38 0 0 1 76 0" />
      <path id={`${id}-bot`} d="M60 60m-38 0a38 38 0 0 0 76 0" />

      <g fill="currentColor" stroke="none" fontWeight={700}>
        <text fontSize="7.5" letterSpacing="1.2">
          <textPath href={`#${id}-top`} startOffset="50%" textAnchor="middle">
            {top}
          </textPath>
        </text>
        <text fontSize="7.5" letterSpacing="1.2">
          <textPath href={`#${id}-bot`} startOffset="50%" textAnchor="middle">
            {bottom}
          </textPath>
        </text>
        <text x="60" y="63" textAnchor="middle" fontSize="10.5" letterSpacing="0.6">
          {center}
        </text>
      </g>
      {/* The cancel bar under the struck word. */}
      <path d="M33 72h54" strokeWidth="1.2" opacity="0.7" />
    </svg>
  );
}
