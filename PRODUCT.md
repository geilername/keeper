# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

**Primary: the photographer.** Someone who has just shot bursts (a portrait
with a blink, a moving subject, a static scene) and wants the best frame of
each burst in Lightroom without culling by hand. Their job in the console:
see that photos are flowing, see what was kept and why, and notice at once
when something needs them (Lightroom unreachable, a stage stalled).

**Secondary: hackathon judges.** They watch the same console during the demo
and may open it on their own phones. They need to follow one photo through the
pipeline and jump to its trace in Dash0. When their needs conflict with the
photographer's, the photographer wins; the demo uses the real tool as it is.

## Product Purpose

Keeper keeps the best frame of every burst. Photos leave the camera over FTP,
are scored (sharpness, exposure, eyes open or closed, similarity), grouped
into bursts, decided (best 5★, keeper 3★, reject 1★) and published to
Lightroom exactly once. The console shows every photo moving through that
pipeline live, explains each rating in words, and links each photo to its
end-to-end trace.

Success: the photographer trusts the decisions without re-checking every
frame, and a failure (Lightroom down, scorer stopped) is visible immediately
and heals without a photo being lost or duplicated.

## Positioning

Every rating comes with its reasons in plain words, measured on the photo
("motion blur (sharpness 0.11, sharpest in burst 0.59)"), and every photo has
one trace across all services, from FTP upload to Lightroom. The pipeline's
behaviour is observable, not asserted.

## Operating Context

- Photos arrive in bursts of about six frames, 0.3 s apart, then a pause.
  A burst is decided after 20 s of quiet (or 90 s at most).
- Pipeline stages a photo passes through: ingested → scored → decided →
  (publishing / retrying) → published, or failed.
- Faults are part of the story: Lightroom returning 503s for up to two minutes,
  or the scorer stopped. The console must make "waiting" and "retrying"
  legible, and show recovery.
- Seen on a laptop (often screen-shared or recorded at laptop resolution) and
  on judges' phones (360 px wide).
- Each photo's Dash0 trace opens in a new tab from the console.

## Capabilities and Constraints

- Served by the `gateway` service: static HTML, CSS and vanilla JS modules in
  `web/static/`, embedded with `go:embed`. No build step, no framework.
- Data: `GET /api/photos`, `GET /api/photos/{sha}` (with burst siblings and
  `trace_url`), `GET /api/bursts/{id}`, live updates over SSE at
  `/api/events`, thumbnails at `/thumbs/{sha}.jpg` (480 px long edge).
- Per photo the console knows: file name, capture time, stage, scorer
  features (sharpness, exposure, eyes, dhash), burst, bucket, rating, reasons,
  publish attempts, last error, Lightroom asset ID.
- Fault controls live on lightroom-mock's admin API (`/admin/faults`); a
  console fault panel is planned and must be clearly marked as a demo control.
- Terminology: photo, burst, best / keeper / reject, stage names as above.
- Undecided: whether the console calls lightroom-mock's admin API directly or
  through the gateway.

## Brand Commitments

- Name: **Keeper** (final).
- Voice: calm and precise. Reasons and states read like museum wall labels:
  short, factual, measured ("eyes closed", "sharpest frame with eyes open").
  Calm by default, unmistakable when action is needed.

## Evidence on Hand

- Demo bursts: three CC0 sets in `demo/sets/` (dogs, no people), sources in
  `demo/SOURCES.md`, with `expected.json` per set.
- Real portrait bursts with blinks will come from the author's own consented
  shoot during the hackathon; none exist yet.
- No real Lightroom account or real camera is used in the demo; no user
  testimonials or metrics exist. Do not fabricate any.

## Product Principles

1. **Photos first.** The frames are the content; everything else supports
   reading them.
2. **Explain every decision.** A rating without its reasons is not finished.
3. **Quiet until it matters.** Normal flow is calm; a problem is impossible
   to miss and says what is happening and what will happen next.
4. **Nothing lost, nothing doubled, and visibly so.** The console shows
   retries and recovery honestly.
5. **One click to the truth.** Any photo leads to its trace.

## Accessibility & Inclusion

- Readable and usable at 360 px wide; dark and light themes.
- State must never be conveyed by colour alone (ratings, stages, errors).
- Respect reduced motion for live updates and state transitions.
