# Firebreak Design QA

## Comparison target

- Source visual truth: `/Users/luoxiaowei/.codex/generated_images/019fbd58-8fef-72a1-8abb-86cbf07196de/exec-5f866522-5387-414d-825d-2664f9a6f920.png`
- Source dimensions: 1536 × 1024 px.
- Implementation screenshots:
  - `docs/audits/2026-08-04-mobile/04-login-after.png` — 390 × 844 px, 390 × 844 CSS viewport, device scale 1.
  - `docs/audits/2026-08-04-mobile/05-overview-after.png` — 375 × 1110 full-page px inside a 390 × 844 CSS viewport; the 15px difference is the browser scrollbar gutter.
  - `docs/audits/2026-08-04-mobile/11-desktop-brand-after.png` — 1440 × 900 px, 1440 × 900 CSS viewport, device scale 1.
- Same-input comparison evidence:
  - `docs/audits/2026-08-04-mobile/12-brand-comparison.png` — source board, login badge and desktop navigation combined into one 1240 × 600 comparison image.
  - `docs/audits/2026-08-04-mobile/13-mobile-before-after.png` — old forced-width layout and new phone layout combined into one 947 × 870 audit image.
- State: authenticated local E2E with populated operational data; login, overview, navigation drawer, fault queue, incident queue, settings table and source-registration modal verified.

## Full-view comparison evidence

The Firebreak containment-gap geometry, navy/steel-blue/orange palette and wordmark hierarchy are preserved from the brand board. The login surface uses the full horizontal badge on a light surface. Desktop and mobile navigation use the rounded-square icon with a light inset boundary so its outer shape remains visible against the navy application rail.

The mobile before/after comparison confirms that the implementation now renders at the requested phone width instead of widening to 720px. Above-the-fold hierarchy is preserved: page identity and search remain first, followed by the four situational metrics and the priority fault queue.

## Focused region comparison evidence

Focused comparison was required for the logo because the full application screenshot renders it at 32px. `12-brand-comparison.png` compares the source lockup with the login badge and dark-navigation variant at readable size. No non-standard visible brand asset was replaced with an SVG, CSS drawing, glyph or placeholder.

## Required fidelity surfaces

- Fonts and typography: the product continues using Avenir Next/PingFang SC fallbacks. Firebreak image wordmark spelling is exact. Mobile titles remain 18–23px with compact operational text at 9–13px; no critical heading is clipped.
- Spacing and layout rhythm: phone shell uses 12px page gutters, 8–14px section gaps, a 2×2 summary grid, and a 108px two-row topbar. Desktop spacing and rail proportions remain unchanged.
- Colors and visual tokens: brand palette maps to existing `--ops-ink`, `--primary`, `--ops-signal` and `--ops-paper` tokens. Semantic critical/warning/success colors remain independent from brand orange.
- Image quality and asset fidelity: final icon is 512px, badge is 960 × 224, and dedicated 180px/32px variants avoid browser downscaling from the full source. Chroma fringe was contracted before final export. Assets use alpha PNG and remain crisp at their consuming sizes.
- Copy and content: all product navigation remains Simplified Chinese. The brand name is consistently Firebreak; operational terminology and page titles are unchanged.

## Comparison history

### Iteration 1 — blocked

- [P2] The dark rounded-square icon blended into the dark sidebar, leaving only the internal bars visible and weakening fidelity to the selected application icon.
- Fix: increased the brand slot to 32px and added a 2px light inset surface plus subtle boundary around the real PNG asset.
- Post-fix evidence: `11-desktop-brand-after.png` and the desktop-navigation crop in `12-brand-comparison.png` show the rounded-square boundary clearly while retaining the source colors.

### Iteration 2 — passed

- No actionable P0/P1/P2 mismatch remains in the brand placement or responsive shell.
- Mobile navigation, fault/incident layouts, settings table containment and modal sizing were retested after the final CSS change.

## Findings

- No actionable P0/P1/P2 findings remain.
- [P3] A formal vector master could improve future print/large-format uses, but the exported PNG sizes are sufficient for all current UI placements.

## Implementation checklist

- [x] Replace legacy SRE Control visible branding with Firebreak.
- [x] Install badge, application icon, favicon and Apple Touch Icon.
- [x] Remove global 720px minimum width.
- [x] Replace disabled narrow sidebar with an accessible drawer.
- [x] Reflow topbar, summaries, fault queue and incident queue.
- [x] Contain dense table scrolling and size modals to the phone viewport.
- [x] Verify authenticated navigation and browser console.
- [x] Pass build and lint.

## Follow-up polish

- Consider an official vector trademark master when brand governance is formalized.
- Consider dedicated mobile card renderers for the most frequently used control-center tables if mobile administration becomes a primary workflow.

final result: passed
