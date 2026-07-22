# Portal design system: a dependency-free UI, and rendering a live pipeline

Covers `services/portal/app/globals.css`, `app/icons.tsx`, `app/journey.tsx` (see `plans/06-sender-portal.md`).

---

## 1. Why no component library

**What it is.** The whole portal UI is one stylesheet of CSS custom properties plus ~25 hand-authored inline SVG components. No Tailwind, no shadcn, no Material, no icon package, no web fonts.

**Why we chose it.** Three reasons that are defensible in an interview:

1. **Egress.** The demo and printer boxes run without internet. A Google-Fonts `@import` or a CDN icon sprite is a hard dependency on a network the deployment target may not have. System font stacks and inline SVG have none.
2. **Bundle and supply chain.** The portal already ships Next + React. An icon package is a few hundred KB of tree-shaking guesswork and one more thing to audit for a project whose entire pitch is "you cannot trust the middle".
3. **`currentColor`.** Inline SVG inherits colour, so light and dark mode need one asset, not two. A raster sprite would need both.

**The honest caveat.** This does not scale to a team. A design system's real value is *consistency under many hands* — a shared vocabulary that stops twelve developers inventing twelve button styles. For a single-author project the tokens do that job; on a real team I would reach for a library and spend the saved time on the tokens layer.

---

## 2. Design tokens and dark mode

Every colour, radius, shadow and easing curve is a custom property on `:root`, with a `@media (prefers-color-scheme: dark)` block that reassigns the *same* names:

```css
:root            { --paper: #fbfaf7; --ink: #14171f; --rule: #e7e4dc; }
@media (prefers-color-scheme: dark) {
  :root          { --paper: #0e1117; --ink: #eceef3; --rule: #262b36; }
}
```

Components only ever read `var(--paper)`. Dark mode is therefore ~40 lines instead of a parallel stylesheet.

**The interesting exception.** The airmail chevron band keeps literal postal red/blue in both schemes (`--band-red`, `--band-blue`) rather than following the text palette. It represents a *physical object*, and physical objects do not invert when you turn the lights off. Semantic tokens invert; representational ones do not — that distinction is worth being able to articulate.

---

## 3. Rendering a distributed pipeline as a route

The tracker is the one screen where the UI and the systems design meet. Five SSE statuses (`submitted → queued → dispatching → printing → delivered`) become five postal stops with a line the document travels along.

Design decisions worth defending:

**Progress is a transform, not a width.** The obvious implementation is `width: 75%` with `transition: width`. Percentage widths resolve against a containing block, so every relayout re-resolves them and can restart or visually snap the transition; it also forces layout on every animation frame. Instead the fill is full-width and scaled:

```css
.journey-rail-fill { width: 100%; transform: scaleX(var(--p)); transform-origin: left; }
```

`--p` is the unitless fraction (`scaleX()` takes a number, not a percentage). Transforms are composited — no layout, no relayout hazard.

**One markup path for both orientations.** Under 560px the same DOM rotates: the rail moves to the left gutter, `scaleX` becomes `scaleY`, and the stops stack. No JS breakpoint, no second component, nothing to keep in sync.

**Positioning context matters.** The rail must start and end on the *first and last icon centres*. It is positioned against a `.journey-rows` wrapper that hugs only the stop list — positioning it against the outer `.journey` made it span the caption below and overshoot the last stop on the vertical layout. The general lesson: an absolutely-positioned decoration should be anchored to the element it decorates, not to a convenient ancestor.

**Timestamps are first-sighting.** `setTimes(prev => prev[status] ? prev : {...prev, [status]: now})`. The status stream is at-least-once (Redis Streams, see `14-redis-streams-consumer-groups.md`), so a status can repeat. Keeping the first sighting means the tracker shows when a job *entered* a stage, not when it last repeated it.

**Each stop explains itself.** The caption under the tracker changes per stop ("Decrypted in the printer's RAM only — never written to a disk"). The security model is the product; the tracker is the best place to state it, and it costs one string per stop.

---

## 4. Accessibility, and why reduced motion is not optional

- **Progress is encoded twice** — in colour *and* in position/label state — so `prefers-reduced-motion: reduce` can kill every animation without losing information. A UI where the animation *is* the signal cannot honour that setting.
- **Icons are decorative.** Every SVG is `aria-hidden="true"`; the meaning lives in adjacent text. This also keeps accessible names clean: a button containing an icon and the word "Search" has the accessible name "Search", not "search-icon Search".
- **Controls stay real.** The recipient picker restyles `<input type="radio">` with `appearance: none` rather than replacing it with a `<div>`; the file dropzone stretches a real `<input type="file">` across the tile at `opacity: 0`. Both keep native keyboard behaviour, native form semantics, and — usefully — remain reachable by the Playwright suite. `display: none` would have broken all three.
- **Responsive tables stay tables.** Below 640px each `<td>` grows a label from its `data-label` attribute and the header row is visually hidden. The element is still a `<table>` with real rows, so screen readers and the e2e selectors both still work.

**The honest caveat.** None of this has been through a screen-reader audit or a contrast checker across every token pair. "I used semantic elements and honoured reduced motion" is the floor, not a claim of WCAG compliance — say so rather than overclaiming.

---

## 5. The line to hold in an interview

> The portal's UI has no dependencies because the deployment target has no egress and the project's whole claim is about not trusting intermediaries. The tracker animates with composited transforms rather than layout properties, and the same DOM rotates between horizontal and vertical layouts in CSS. Everything is token-driven, so dark mode is a variable reassignment — and every animation is disabled under `prefers-reduced-motion`, which is only safe because progress is also encoded in colour and position.
