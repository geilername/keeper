---
version: 1
slug: "web-static-index-html"
primary_target: "web/static/index.html"
related_targets: []
---

# Keeper console — flow view and photo sheet

Mode: Operate. Audience: the photographer first (is it flowing, what was kept and why), judges second (follow one photo to its trace). Seen on a laptop, often screen-shared, and on phones at 360 px. This round: flow view + photo record sheet. Fault panel deferred.

## Direction contract

THESIS: The console is a printer's exposure record of the shoot: every stage is a step on one tonal ramp, every photo a print with measured readings, and state is carried by tone and hatching, never by coloured badges. It refuses the category default: a dark ops dashboard of KPI tiles, traffic-light pills and photos shrunk to row icons.

OWN-WORLD: Paper ground (zone IX), ink (zone 0), an eleven-step neutral zone ramp as the only tonal tokens; one dichroic magenta used for selection and focus only. Hatching is the state layer: diagonal hatch density = photos waiting at a stage, 90° cross-hatch = conflict (retrying or failed). Ruled record tables with 1 px ink rules, square corners, tracked condensed caps for legends, tabular numerals for quantities, readings in stop notation (−⅓, +1⅔). Photos sit unhatched in a two-step mask. Dark theme = the ramp inverted.

STORY: At a glance the photographer sees whether the shoot is flowing (the stage ramp with counts and hatch load), the newest burst as a row of prints with readings relative to the burst's best, which frame was kept and why, and any stalled photo cross-hatched with its next retry time. Selecting a print opens its record sheet: rating, reasons, readings, journey to scale, Open trace in Dash0.

FIRST VIEWPORT: 1440×900. Top bar: KEEPER in condensed caps, Flow tab, a plain status sentence on the right ("Flowing · 18 photos · last arrival 4 s ago"). Below: a thin zone wedge over five stage cells spanning the width (Ingested, Scoring, Burst window, Publishing, In Lightroom), count in large tabular numerals, hatch fill by load. Main: left, bursts newest first as record blocks (ruled header: burst, frames, time span, window countdown or decided; then prints ~200 px wide with bucket, stars and stop readings under each). Right, a 400 px record sheet for the selected print, its primary action Open trace in Dash0 as a black button with a Δ cell. Signature move: the hatched stage ramp plus stop-delta readings.

FORM: Exposure Record, the competitive catalog challenger the user chose over the assigned direction (my candidate 7, The Hang). Kept from the round: journey bar drawn to real seconds per stage, burst window countdown, every reason beside its measurement. seed:0adf20941c87e68c

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

## Deviations from FIRST VIEWPORT (cited)
- No Flow tab: this round ships a single view; a lone tab is chrome with nothing to switch to. Tabs arrive with the fault panel or a Lightroom view.
- Four stage cells, not five: "ingested" means "waiting for the scorer", so Ingested and Scoring are one step (Scoring, zone VIII).
- The floating zone wedge was replaced by a zone-toned band and numeral on each stage cell (finish review), so the stages are the ramp.

## Open decisions
- Fault panel (next round): calls lightroom-mock admin directly or via gateway.
