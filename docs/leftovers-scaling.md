# Large leftover lists

The leftovers table is virtualized by MyGo's `ui.Table` / `ui.ListState`.
It builds viewport rows and a small overscan, rather than creating a control for
every result. The complete scan results and grouped tree still live in memory.

The view now adds:

- A single header row with the app name, search, compact result count, and an
  **Options** menu. Filtering, bulk selection and expand/collapse actions live
  in that menu. Total size stays in the count's tooltip; selected count and size
  appear in the footer, keeping the header free of duplicate size text.
- Search by name, full path (including the registry hive), or discovery reason.
  Search ignores case and surrounding whitespace. Ctrl+F focuses the search.
- An **Options → Possible matches only** filter to review uncertain items, which
  remain unselected by default. The menu button reads **Possible only** while
  this filter is active.
- **Select shown** and **Deselect shown** while filtering. These affect every
  matching result, including off-screen and collapsed items. Group checkboxes
  follow the same scope. Other selections survive filtering.
- A single footer row at every window width, with the selected count and size
  on the left and compact Remove/Skip buttons on the right. Both buttons use
  32-DIP heights and 6-DIP corner radii. Remove has a cleanup icon and no fixed
  minimum width; Skip uses a 60-DIP minimum width. Hidden selections are
  highlighted in the summary. Full backup and removal explanations remain in
  accessible labels and tooltips, and Remove's accessible name includes the
  complete selection count and size.
- Search results that expand automatically, a stable dialog height, and an empty
  state with **Clear filters**. Clearing filters restores the unfiltered folds.
- Home, End, arrow, and Page Up/Down scrolling while the table has focus. Space
  toggles a focused checkbox. **Collapse all** also collapses sections with
  ungrouped items, and returns the viewport to the top.
- Shorter large counts and warning icons at narrow widths so filenames retain
  space. Full paths, uncertainty explanations, and counts have accessible labels
  or tooltips.

Visible-row flattening is cached until the data, filter, or folds change. Group
selection and size totals are calculated once per frame and shared across cells.
Leaf identities use the original result index, avoiding duplicate display-label
collisions. Closing the dialog releases its tree, filter, and selection data.

## Validation

`leftovers_view_test.go` covers 0, 1, 100, 5,000, and 50,000 results; mixed
filesystem/registry results and registry-only results; expanded and collapsed
trees; 5,000 singleton groups; duplicate labels; first/last-row reachability;
keyboard scrolling and checkbox toggling; case-insensitive full-path/reason
search; combined filters; empty-filter recovery; selection of off-screen items;
filtered group/bulk selection; hidden selections; and live size updates with
removal disabled during measurement. The 5,000-result flows run in both themes
at 720×480 and 1060×700. Existing layout tests also exercise widths up to 1440.

The tests assert fewer than 50 built leaf labels for the expanded large lists.
The first and final rows remain reachable above the persistent footer. A native
Windows preview with synthetic results was also inspected, including search and
the selection warning. No real app was uninstalled or cleaned during this review.

Measured on Windows amd64, Intel Core i5-12400F, Go 1.27.3. After values are medians
of three runs using `-benchtime=300ms`; before values are one baseline run.

| Headless operation | Before | After | After bytes/frame |
| --- | ---: | ---: | ---: |
| 5,000 registry results, groups initially folded | 0.295 ms | 0.285 ms | 4,753 |
| 5,000 registry results, fully expanded | 0.530 ms | 0.532 ms | 17,930 |
| 50,000 registry results, groups initially folded | 0.858 ms | 0.682 ms | 4,760 |
| 50,000 registry results, fully expanded | 1.158 ms | 0.945 ms | 17,954 |

Alternating between an all-matching search and a one-result search, including
tree rebuilding and headless layout, took about 4.94 ms per change at 5,000 mixed
results and 19.0 ms at 50,000. Search rebuilds allocate with the matching dataset;
idle frame allocations stay roughly constant as the result count grows.

These timings measure UI build/layout, not GPU presentation, scrolling frame
pacing, disk scans, or end-to-end cleanup. They do not establish full screen
reader compatibility or accessibility compliance. Large scans still require
memory proportional to their results, and selection summaries remain linear
in result count. The 50,000-result search can take more than a 60 Hz frame.

Reproduce:

```powershell
go test ./... -count=1
go vet ./...
go test -run '^$' -bench '^BenchmarkLeftover(Scale|Search)$' -benchmem -benchtime=300ms -count=3
go build .
```

Set `SNAPSHOTS` to an existing directory before running the tests to save UI
images. This review's screenshots, report, and raw benchmark output are in the
git-ignored `build/leftover-review/` folder.
