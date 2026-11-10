---
name: Keeper
description: The console as a printer's exposure record of the shoot; tone and hatching carry state.
colors:
  zone-0: "#0f0f0e"
  zone-1: "#1d1d1c"
  zone-2: "#2f2f2d"
  zone-3: "#434341"
  zone-4: "#575755"
  zone-5: "#6e6e6b"
  zone-6: "#888884"
  zone-7: "#a3a39f"
  zone-8: "#bfbfbb"
  zone-9: "#dadad5"
  zone-10: "#efefea"
  sheet-paper: "#f7f7f3"
  sheet-paper-dark: "#262624"
  mask-black-dark: "#000000"
  hatch-ink: "rgb(15 15 14 / .42)"
  hatch-ink-dark: "rgb(239 239 234 / .38)"
  dichroic-magenta: "#c2177b"
  dichroic-magenta-dark: "#ff5cb4"
  filter-ink: "#ffffff"
typography:
  display:
    fontFamily: "Archivo, Arial Narrow, sans-serif"
    fontSize: "26px"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "0.06em"
    fontVariation: "'wdth' 62"
  headline:
    fontFamily: "Archivo, Arial Narrow, sans-serif"
    fontSize: "48px"
    fontWeight: 500
    lineHeight: 1
    fontVariation: "'wdth' 62"
  status:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "18px"
    fontWeight: 400
    lineHeight: 1.35
  status-word:
    fontFamily: "Archivo, Arial Narrow, sans-serif"
    fontSize: "20px"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "0.08em"
    fontVariation: "'wdth' 70"
  title:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "17px"
    fontWeight: 600
    lineHeight: 1.3
  body:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "15px"
    fontWeight: 400
    lineHeight: 1.5
    fontFeature: "'tnum'"
  body-small:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "13.5px"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "Archivo, Arial Narrow, sans-serif"
    fontSize: "11.5px"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "0.14em"
    fontVariation: "'wdth' 70"
  stamp:
    fontFamily: "Archivo, Arial Narrow, sans-serif"
    fontSize: "11px"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "0.12em"
    fontVariation: "'wdth' 70"
  action:
    fontFamily: "Archivo, Arial Narrow, sans-serif"
    fontSize: "13px"
    fontWeight: 600
    lineHeight: 1
    letterSpacing: "0.14em"
    fontVariation: "'wdth' 75"
  mono-name:
    fontFamily: "Red Hat Mono, ui-monospace, monospace"
    fontSize: "17px"
    fontWeight: 500
    lineHeight: 1.3
  mono-value:
    fontFamily: "Red Hat Mono, ui-monospace, monospace"
    fontSize: "13.5px"
    fontWeight: 400
    lineHeight: 1.4
  mono-reading:
    fontFamily: "Red Hat Mono, ui-monospace, monospace"
    fontSize: "12.5px"
    fontWeight: 400
    lineHeight: 1.45
rounded:
  none: "0px"
spacing:
  gutter: "clamp(16px, 2.6vw, 40px)"
  sheet: "400px"
  band: "8px"
  mask: "3px"
  mask-large: "4px"
  row: "8px"
  print-min: "146px"
  burst: "28px"
components:
  print-frame:
    backgroundColor: "{colors.zone-0}"
    padding: "{spacing.mask}"
    rounded: "{rounded.none}"
  print-frame-held-back:
    backgroundColor: "transparent"
    padding: "2px"
    rounded: "{rounded.none}"
  bucket-best:
    backgroundColor: "{colors.zone-0}"
    textColor: "{colors.zone-10}"
    typography: "{typography.stamp}"
    rounded: "{rounded.none}"
    padding: "4px 6px 3px"
  bucket-keeper:
    backgroundColor: "transparent"
    textColor: "{colors.zone-0}"
    typography: "{typography.stamp}"
    rounded: "{rounded.none}"
    padding: "4px 6px 3px"
  bucket-reject:
    backgroundColor: "transparent"
    textColor: "{colors.zone-4}"
    typography: "{typography.stamp}"
    rounded: "{rounded.none}"
    padding: "4px 6px 3px"
  action-trace:
    backgroundColor: "{colors.zone-0}"
    textColor: "{colors.zone-10}"
    typography: "{typography.action}"
    rounded: "{rounded.none}"
    padding: "15px 16px 14px"
  action-trace-cell-active:
    backgroundColor: "{colors.dichroic-magenta}"
    textColor: "{colors.filter-ink}"
    width: "48px"
  record-sheet:
    backgroundColor: "{colors.sheet-paper}"
    textColor: "{colors.zone-0}"
    padding: "20px 24px 40px"
    width: "{spacing.sheet}"
---

# Design System: Keeper

## Overview

**Creative North Star: "The Exposure Record"**

The console is a printer's exposure record of a shoot. Every pipeline stage is a step on one tonal ramp, every photo is a print with measured readings beneath it, and state is carried by tone and hatching rather than by coloured badges. A photo darkens toward zone 0 as it develops from the scorer into Lightroom; the dark theme is the same ramp read in reverse.

The surface is paper and ink: a pale warm-grey ground, black 1px rules, square corners, condensed tracked capitals for legends and tabular monospace for every quantity. The only colour in the system is a dichroic magenta that marks selection and focus. Load and conflict are drawn as hatching in dedicated strips and swatches, never behind words, and every hatch pattern is explained by an on-screen key.

Density is that of a ruled ledger: compact rows, generous paper between bursts, photos large enough to judge. Calm is the default; a conflict changes ink weight and pattern, and is stated in words in the status sentence, so it never depends on colour.

**Key Characteristics:**
- An eleven-step zone ramp (0 to X) is the only neutral scale; tone carries stage.
- Hatching is the state layer: diagonal density for load, 90° cross-hatch for conflict.
- One accent, dichroic magenta, reserved for selection and focus.
- Prints sit in a black mask; rejects are held back in a dashed outline.
- Readings are deltas against a reference frame, in thirds of a stop.
- Square corners, 1px ink rules, condensed tracked caps for names, tabular mono for measurements.

## Colors

A neutral zone ramp from near-black to warm paper, plus a single dichroic magenta that never carries meaning beyond "this is selected or focused".

### Primary
- **Dichroic Magenta** (dichroic-magenta; dichroic-magenta-dark in the dark theme): text selection, the selected print's outline, the 1px hover preview of that outline, `:focus-visible` rings, and the Δ cell of the trace action on focus or press. Text on it uses filter-ink (white in light, zone 1 in dark).

### Neutral
- **Zone 0, Ink** (zone-0): body text, rules that close a section (stage row, burst header, record tables), the best stamp, the trace action, and the print mask. The final stage band (In Lightroom).
- **Zone 1** (zone-1): the dark-theme ground.
- **Zone 2** (zone-2): the photo well behind an image while it loads; the first stage band in dark.
- **Zone 3** (zone-3): the Publishing band; hairline rules in dark.
- **Zone 4, Secondary Ink** (zone-4): legends, metadata, readings, the reject stamp and the held-back outline.
- **Zone 5** (zone-5): the divider inside the trace action, both themes.
- **Zone 6** (zone-6): the Burst window band; scrollbar thumbs.
- **Zone 7** (zone-7): secondary ink in dark.
- **Zone 8, Hairline** (zone-8): row rules, the countdown track, the Scoring band.
- **Zone 9** (zone-9): strong rules in dark.
- **Zone 10, Paper** (zone-10): the light-theme ground; ink in dark.
- **Sheet Paper** (sheet-paper; sheet-paper-dark): the record sheet's raised surface, half a step off the ramp so the sheet reads as a separate leaf without a shadow.
- **Hatch Ink** (hatch-ink; hatch-ink-dark): the translucent ink of load hatching only.
- **Mask Black, dark** (mask-black-dark): in the dark theme the print mask drops below zone 0 so it stays a step under the zone 1 ground.

### Named Rules
**The Zone Ramp Rule.** Tone carries state. Every neutral comes from zone-0 through zone-10; the only off-ramp values are the sheet paper, the dark-theme mask black and the hatch inks, each with the single job named above.

**The Dichroic Filter Rule.** Magenta appears only on selection (including the 1px hover preview of a print's selection outline), `:focus-visible` and `:active`. It never marks a stage, a bucket, a reading or an error.

**The Inverted Ramp Rule.** The dark theme is the ramp inverted, not a new palette: ground, ink, rules and stage bands swap ends of the same eleven steps.

## Typography

**Display Font:** Archivo, variable width 62–125 and weight 100–900, self-hosted (with Arial Narrow, then sans-serif)
**Body Font:** Archivo at normal width (with system-ui)
**Label/Mono Font:** Red Hat Mono, weight 400–600, self-hosted (with ui-monospace)

**Character:** One grotesque family stretched two ways: condensed and tracked for everything that names, normal width for sentences. Red Hat Mono carries every value that is measured or identified; stage counts stay in condensed Archivo.

### Hierarchy
- **Display** (600, 26px, line-height 1, width 62%, uppercase, tracked .06em): the KEEPER wordmark only.
- **Headline** (500, 48px, line-height 1, width 62%; 34px at 640px and below): stage counts.
- **Status** (400, 18px, line-height 1.35; 16px on phones) with its **status word** (600, 20px, width 70%, uppercase, tracked .08em): the plain status sentence in the top bar, led by its state word (Flowing, Idle, or the alert).
- **Title** (600, 17px, line-height 1.3): burst headings and the empty-state heading (20px).
- **Body** (400, 15px, line-height 1.5, tabular numerals on): reasons, notes, sentences; secondary lines at 13.5px.
- **Label** (600, 11.5px, width 70%, uppercase, tracked .14em, secondary ink): stage names, record table headers, section legends (Why, Journey).
- **Stamp** (600, 11px, width 70%, uppercase, tracked .12em): bucket stamps.
- **Action** (600, 13px, width 75%, uppercase, tracked .14em): the trace action.
- **Mono** (Red Hat Mono 500 17px for the record's file name; 500 13px for print numbers; 400 13.5px for table values; 400 12.5px for readings): every measured value, identifier and reading.

### Named Rules
**The Two Voices Rule.** Names are condensed tracked capitals; measurements and identifiers are Red Hat Mono. Every figure is tabular (tnum is on globally); the only large numerals, stage counts, are condensed Archivo.

**The Thirds Rule.** Exposure readings are deltas against the burst's reference frame (the best, or a stand-in until decided), rounded to thirds of a stop: the sign is always shown (+, −, ± for zero) and the fraction is set as n⁄3 at .86em. Sharpness deltas are signed to two decimals. The reference frame itself reads "ref" with its absolute values.

## Layout

A single scrolling flow with a fixed right-hand record. The page gutter is fluid (gutter). Below the top bar, the stage ramp spans the full width as four equal cells (18px column gap) closed by a 1px ink rule, with the hatch key right-aligned beneath. The work area is a two-column grid: bursts on the left, the record sheet (sheet width) sticky at full viewport height on the right.

Bursts stack newest first, 28px apart at the head. Each burst is a ruled header (name, frame count and time span, state on the right, a 3px countdown bar while the window is open) over a print grid that auto-fills columns of at least print-min with 22px by 18px gaps. Table and list rows share an 8px vertical rhythm.

At 1080px and below the record becomes a panel that slides in from the right (min(440px, 100vw)) over the flow, with a close button. At 640px and below the top bar stacks, the stages become a 2 by 2 grid with hairline rules under each cell, the burst header becomes one column, and prints run two to a row.

### Named Rules
**The Developing Ramp Rule.** Stages read left to right as one ramp: Scoring (zone VIII), Burst window (VI), Publishing (III), In Lightroom (0). Each cell carries an 8px band in its zone tone across its top and its zone numeral in mono at the right of its legend; in dark the tones invert (II, IV, VII, X) while the numerals keep their names.

## Elevation & Depth

The system is flat paper. Depth comes from rules and from the record sheet sitting on slightly lighter sheet paper behind a 1px ink rule. There is exactly one shadow: when the record slides over the flow at 1080px and below, it casts a soft leftward shadow so the panel reads as a leaf laid on top.

### Shadow Vocabulary
- **Overlay leaf** (`box-shadow: -12px 0 32px rgb(0 0 0 / .18)`): the narrow-screen record panel only.

### Named Rules
**The Flat Paper Rule.** Nothing at rest casts a shadow. Separation is a rule, a tone step or the black mask.

## Shapes

Every corner is square (rounded none), including prints, stamps, buttons and swatches. Borders are 1px: ink for rules that close a section, hairline for rows. Inset 1px box-shadows draw outlines on swatches and the close button so they never shift layout.

### Named Rules
**The Hatch Rule.** Hatching lives only in dedicated strips and swatches, never under text. Diagonal hatching at 135° is load, in three densities (10px, 6px and 3px period) in hatch ink; a 45° plus 135° cross-hatch in full ink is conflict (retrying or not published). The same two patterns appear in the stage load strip, the key, the status alert mark, a print's conflict line and the journey bar.

**The Mask Rule.** A photo is never hatched or tinted. It sits in a black mask (3px; 4px on the record's large print). A reject is held back: no mask, a 1px dashed outline in secondary ink, the photo at 70% opacity.

## Components

### Stage cell
The signature. An 8px top band in the stage's zone tone; legend with the zone numeral; the count in headline type; a one-line note (bold when in conflict); and a 26px load strip on the right edge (16px on phones), separated by a hairline, filled with hatching by load or cross-hatch on conflict (its edge rule turns to ink). Hatch fills transition over .6s.

### Hatch key
A right-aligned line under the stages: a 14px swatch for "photos waiting, denser = more" and one for "retrying or not published", each outlined with an inset 1px ink line.

### Print
- **Frame:** 3:2, black mask, image cropped to cover; the zone 2 well shows while loading.
- **Caption:** frame number in mono on the left, bucket stamp on the right; below, the readings line in mono secondary ink; below that, where the photo is (stage word, or the retry line with next try and attempt count).
- **Selection:** magenta outline offset 3px: 1px on hover, 3px when selected or keyboard-focused.
- **Conflict:** the location line turns to ink at 600 weight, led by a 10px cross-hatch swatch.
- **Arrival:** a new print develops up out of the paper once (1.6s, from washed-out and grey to full), disabled under reduced motion.

### Bucket stamps
Condensed tracked caps with a star glyph from the inline SVG set, 1px border, square.
- **Best:** inverted, ink fill with paper text.
- **Keeper:** outlined in ink, transparent.
- **Reject:** dotted border, secondary ink.
- **Pending (Scoring, Scored):** hairline border, secondary ink.

### Trace action
A full-width ink bar with paper text in the action voice and a 48px Δ cell holding the open-in-new-tab icon, split by an inset zone 5 rule. Hover underlines the words; focus or press fills the cell with magenta. Disabled: transparent with a hairline outline and secondary ink.

### Record sheet
Sheet paper, ink rule on the left, sticky header with the file name in mono and metadata beneath. Then the trace action, the trace id in small mono (only the hex id may break), the large print, and a record table: labels left, mono values right with a sans note line beneath, rows on hairlines under an ink top rule. Sections "Why" and "Journey" use label legends; reasons are ruled rows.

### Journey bar
A 14px bar of segments drawn to real seconds per stage, 2px apart: solid ink for completed stages, ground with diagonal hatch for the stage in progress, cross-hatch for a conflict in progress. A list beneath gives each stage with its duration in mono.

### Lightroom line
A ruled strip under the stage ramp (hairlines above and below) that says where published photos go: the label "Lightroom", then a state sentence led by a state word in condensed caps (Connected, No collection, Not connected, Sign-in expired, Mock), the collection as a native select, and "Connect Lightroom" in the trace action's ink bar with its Δ cell. The select is square, ground-filled, outlined with an inset 1px ink line (2px on hover, hairline when disabled) with the chevron from the icon set. A state that holds photos back (no collection, expired, not connected) takes the status line's cross-hatch mark before its word and says what waits in words. At 860px and below the select and action drop under the sentence; at 640px and below everything stacks and the select is full width.

### Find line
A ruled strip under the Lightroom line (it shares that line's rule when directly beneath it) for searching every photo Keeper has seen: the label "Find", a square field outlined with an inset 1px ink line (2px on hover), two native selects in the Lightroom line's style (rating, stage), and "Find" in the trace action's ink bar with a magnifier in its Δ cell. Beneath, "Try" and sample queries set in mono, underlined in hairline, because they are query syntax. `/` focuses the field; Escape clears it. At 860px and below the selects drop under the field; at 640px and below everything stacks and the selects share a row.

### Search results
While a search is active, its results replace the bursts; the stage ramp and status line stay live above, so a conflict is never hidden by a search. The block opens with a header ruled like a burst header: the count ("7 photos found", or "40 of 312 photos found, best matches first"), what was searched with the query in mono, and "Clear search". Results are ordinary prints in the print grid, with a caption suited to a frame seen outside its burst: the file name on its own line in mono (never cut), the bucket stamp, the first reason in ink (with "+n" for more), and the location line. Readings are left to the record sheet, because without the burst there is no reference frame. An unreadable query or an unavailable index is said in words under the header; it never uses the conflict hatch, which belongs to the pipeline.

### Status line
The top bar's right-hand sentence: a status word in condensed caps, then plain words and counts. On a conflict the word becomes the alert headline, preceded by a cross-hatch mark, and the sentence says what is happening and what happens next.

## Do's and Don'ts

### Do:
- **Do** take every neutral from the zone ramp and let tone say where a photo is.
- **Do** draw load and conflict with the two hatch patterns, in their own strip or swatch, and keep the key visible wherever they appear.
- **Do** set readings, values and identifiers in Red Hat Mono, keep every figure tabular, and give every exposure reading as a signed delta in thirds of a stop.
- **Do** put photos in the black mask and hold rejects back with the dashed outline.
- **Do** say a conflict in words as well as pattern: the status sentence and the print's location line name it.

### Don't:
- **Don't** introduce coloured badges, traffic-light pills or a second accent; state is tone, pattern and words.
- **Don't** use magenta for anything but selection, focus and press.
- **Don't** place hatching behind text or over a photo.
- **Don't** round corners or add shadows to resting surfaces.
- **Don't** shrink photos to row icons; a print stays at least 146px wide.
