# Changelog

## 0.0.4 — 2026-10-31

- Group leftovers into expandable Files and folders and Registry sections,
  with selection and size totals for each group.
- Search leftovers by name, full path or reason, and filter to possible matches.
- Keep the results header and footer on one row, with bulk actions in Options
  and compact Remove and Skip buttons.
- Show selection counts, sizes and hidden selections clearly while filtering.
  Bulk selection applies to all matching results, including off-screen items.
- Fix keyboard scrolling and collapsing sections with ungrouped entries.
- Cache visible tree rows and use stable item identities for large result lists,
  with regression coverage through 50,000 entries.

## 0.0.3 — 2026-10-31

- Find more of what apps leave behind: XDG folders in your home (`.config`, `.cache`,
  `.local`), Temp folders, updater folders, npm commands and packages, link handlers
  such as `app://`, notification servers, and the records Windows keeps of programs it ran.
- Offer an app's installer in Downloads, never selected for you.
- Scan for leftovers about 3.6 times faster.

## 0.0.2 — 2026-10-31

- Polish spacing and alignment across the app list, navigation, details and dialogs.
- Compact the leftovers header and footer, keeping more room for the items being reviewed.
- Improve narrow-window layouts, long-text handling and scrolling without clipped rows.
- Improve startup, app loading and background leftover scanning, with bounded icon caching.
- Refresh the README screenshots and add layout regression coverage for both themes.

## 0.0.1 — 2026-10-31

First public release.

- List every installed app (for everyone and for you), with search, sorting, and views for
  recently installed, large, and uninstaller-missing apps.
- Run an app's own uninstaller, quietly when it supports it, and wait for every process it starts.
- Scan for leftover folders, files, shortcuts and registry entries, with sure matches
  pre-selected and possible matches flagged.
- Remove leftovers safely: files to the Recycle Bin, registry entries backed up to `.reg` files first.
- Clean up broken entries whose uninstaller is missing.
- Native Windows 11 look: Mica, Fluent icons, accent color, light and dark themes.

Builds are not code-signed yet; SmartScreen may warn on first run (**More info → Run anyway**).
