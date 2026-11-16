# Performance measurements

Measured on Windows amd64, Intel Core i5-12400F, Go 1.27.3. The baseline is commit
`fd96f90`. Values below are medians of three runs with `-benchtime=600ms` on the
same machine. Filesystem measurements use warm caches and temporary synthetic
data; these are component measurements, not promises for every installed app.

| Operation | Before | After | Allocated bytes before → after |
| --- | ---: | ---: | ---: |
| Create startup model | 17.512 ms | 0.071 ms | 8,472,725 → 75,272 |
| Read installed apps (65 entries) | 8.616 ms | 6.678 ms | 372,396 → 405,997 |
| Filter 2,000 apps | 53.845 µs | 16.275 µs | 0 → 0 |
| App-list frame (2,000 rows) | 0.372 ms | 0.364 ms | 5,031 → 4,695 |
| Leftover-results frame (2,000 entries) | 0.414 ms | 0.381 ms | 34,585 → 12,250 |
| Prune 2,000 nested leftovers | 2.321 ms | 0.451 ms | 163,344 → 134,608 |
| Size a tree of 2,000 files | 1.652 ms | 1.140 ms | 502,247 → 8,318 |
| Inspect a shortcut target | 45.867 µs | 26.147 µs | 16,574 → 711 |
| Synthetic scan, including sizing | 0.195 ms | 0.181 ms | 14,082 → 12,989 |

The app-list frame timing difference is small enough to treat as approximately
unchanged. Headless frame measurements cover the UI framework's build/layout
path, not real GPU presentation or monitor latency. Model creation is about 245×
faster, but is only one part of startup. Paired measurements of native first-frame
presentation were not completed: desktop verification was stopped with Escape.
UAC, Windows process loading, font initialization, GPU initialization, and cold
storage can still dominate startup.

Registry loading now prepares display labels and directory availability once,
moving filesystem checks and formatting out of rendering. This increases its
transient allocations by about 34 KB while reducing repeated work during use.

## Implementation

- The title bar uses a generated 64px asset instead of decoding and sampling the
  original 1024px logo twice during startup. The branding source stays unchanged.
- At most eight installed-app registry readers run across four branches, keeping
  uneven branches from serializing the entire load. Results remain deterministic.
- Discovery and size measurement are separate. Protected-folder checks and
  certainty decisions finish before matches appear. Selection is available while
  sizes are measured; removal becomes available when measurement completes.
- Discovery uses at most four workers. Sizing also uses at most four workers,
  with a fast path for zero or one filesystem result, rather than allocating a
  goroutine for every leftover. Closing a dialog cancels its scan. Generation
  checks prevent older background results from replacing newer state.
- Directory measurement reads sizes from Windows enumeration metadata, avoiding
  per-file objects, sorting, and file-path allocations. It supports long paths
  and skips reparse points. Totals remain logical file sizes, not allocated disk
  blocks, matching the previous behavior for ordinary files.
- Nested-item pruning uses ancestor maps instead of all-pairs comparisons.
  Certainty and registry-hive boundaries are preserved. Sort keys are prepared
  once instead of formatting and case-folding paths inside every comparison.
- Shortcut inspection reuses bounded 64 KB buffers and prepares target encodings
  once per scan. It handles UTF-8/ANSI-style text and both UTF-16 alignments.
  Unrelated startup entries no longer trigger executable-existence probes.
- A dedicated COM worker extracts shell icon pixels directly, avoiding UI-thread
  shell calls and a PNG encode/decode round trip. Publications are batched. The
  cache retains at most 128 entries, reducing its count at high DPI toward a
  2 MiB base-pixel budget; renderer textures/mipmaps have their own overhead.
- Navigation counts, app labels, and leftover row grouping are cached. Filtered
  slice tails and dismissed dialog data are cleared to release references.

## Reproduce

```powershell
go test ./... -count=1
go vet ./...
go test -run '^$' -bench . -benchmem -benchtime=600ms -count=3
go tool mygo build
```

Regenerate the title-bar asset after changing the source logo:

```powershell
go generate ./...
```

The regression suite covers discovery before measurement, repeated scans,
cancellation, bounded workers and caches, long paths, certainty-aware pruning
against the original algorithm, registry-hive isolation, shortcut encodings,
native icon extraction, and light/dark/narrow headless UI flows. The optional
symlink assertions could not run on this machine because the account lacks the
required privilege. All other tests, `go vet`, and the production executable and
installer build passed.

The race detector could compile with the available Zig compiler and Windows SDK
library, but ThreadSanitizer failed at runtime while reserving its shadow memory
(Windows error 87). Race-detector validation is therefore incomplete; rerun
`go test -race ./...` with a compatible Windows C toolchain/environment.

Raw local benchmark output is in `build/performance-before.txt`,
`build/performance-extra-before.txt`, `build/performance-after.txt`, and
`build/performance-loadapps-after.txt`. The last file supersedes the registry-load
row in the earlier after run. These local build artifacts are excluded from git.
