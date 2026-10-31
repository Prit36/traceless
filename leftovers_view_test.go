package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows/registry"
)

func leftoverAction(tt *ui.Tester, label string) error {
	if err := tt.Click("Leftover options"); err != nil {
		return err
	}
	return tt.ChooseMenuItem(label)
}

// Synthetic paths are display data only: these tests never scan or remove them.
func largeLeftoverModel(n int, registryOnly bool) *model {
	m := sampleModel()
	m.target, m.phase = m.apps[3], phaseResults
	m.leftovers = make([]Leftover, n)
	for i := range m.leftovers {
		l := Leftover{Kind: KindFile, Path: fmt.Sprintf(`C:\Users\Ada\AppData\Local\VideoLAN\cache\entry-%05d.bin`, i),
			Reason: "Files named after the app", Sure: i%5 != 0, Selected: i%5 != 0, Size: 1024}
		if registryOnly || i%2 != 0 {
			l.Kind, l.Hive = KindValue, registry.CURRENT_USER
			l.Path, l.Value, l.Reason, l.Size = `Software\VideoLAN\VLC`, fmt.Sprintf("entry-%05d", i), "Settings named after the app", 0
		}
		m.leftovers[i] = l
	}
	return m
}

func TestLeftoverLargeList(t *testing.T) {
	for _, registryOnly := range []bool{false, true} {
		for _, size := range [][2]int{{720, 480}, {1060, 700}} {
			for _, dark := range []bool{false, true} {
				t.Run(fmt.Sprintf("registry=%v/%dx%d/dark=%v", registryOnly, size[0], size[1], dark), func(t *testing.T) {
					m := largeLeftoverModel(5000, registryOnly)
					tt := ui.NewTester(m.view, size[0], size[1])
					tt.SetDark(dark)
					tt.Frame()
					snapshot(t, tt, fmt.Sprintf("large-initial-registry-%v-%dx%d-dark-%v", registryOnly, size[0], size[1], dark))
					if err := leftoverAction(tt, "Expand all"); err != nil {
						t.Fatal(err)
					}
					assertVirtualLeftovers(t, tt, m)
					list := visibleBox(t, tt, "Leftovers", size)
					tt.Scroll(list.X+list.W/2, list.Y+list.H/2, 0, 1e7)
					visibleBox(t, tt, "entry-04999", size)
					assertVirtualLeftovers(t, tt, m)
					snapshot(t, tt, fmt.Sprintf("large-end-registry-%v-%dx%d-dark-%v", registryOnly, size[0], size[1], dark))
					if err := leftoverAction(tt, "Select none"); err != nil {
						t.Fatal(err)
					}
					for _, l := range m.leftovers {
						if l.Selected {
							t.Fatal("Select none missed an offscreen item")
						}
					}
					if err := leftoverAction(tt, "Select all"); err != nil {
						t.Fatal(err)
					}
					for _, l := range m.leftovers {
						if !l.Selected {
							t.Fatal("Select all missed an offscreen item")
						}
					}
					if err := leftoverAction(tt, "Collapse all"); err != nil {
						t.Fatal(err)
					}
					tt.Frame()
					visibleBox(t, tt, "Choose all of Registry", size)
					if err := leftoverAction(tt, "Expand all"); err != nil {
						t.Fatal(err)
					}
					m.rowList.ScrollToEnd()
					tt.Frame()
					visibleBox(t, tt, "entry-04999", size)
				})
			}
		}
	}
}

func assertVirtualLeftovers(t *testing.T, tt *ui.Tester, m *model) {
	t.Helper()
	first, last := m.rowList.Visible()
	if last-first > 40 {
		t.Fatalf("too many visible rows: %d..%d", first, last)
	}
	built := 0
	for _, text := range tt.Texts() {
		if strings.HasPrefix(text, "entry-") {
			built++
		}
	}
	if built == 0 || built > 50 {
		t.Fatalf("expected only viewport rows to be built, got %d", built)
	}
}

func TestLeftoverSearchAndSelection(t *testing.T) {
	for _, size := range [][2]int{{720, 480}, {1060, 700}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m := largeLeftoverModel(5000, false)
			tt := ui.NewTester(m.view, size[0], size[1])
			tt.Frame()
			original := visibleBox(t, tt, "Leftovers", size)
			m.rowList.ScrollToEnd()
			tt.Frame()
			tt.Key(ui.Cmd, ui.KeyF)
			if !tt.Focused("Search leftovers") {
				t.Fatal("Ctrl+F did not focus leftovers search")
			}
			tt.Type(" ENTRY-04999 ")
			if len(m.matched) != 1 || m.matched[0] != 4999 || !tt.HasText("entry-04999") {
				t.Fatalf("search did not reveal the last entry: %v", m.matched)
			}
			filtered := visibleBox(t, tt, "Leftovers", size)
			if original != filtered {
				t.Fatalf("dialog moved while searching: %+v -> %+v", original, filtered)
			}
			note := visibleBox(t, tt, "3999 selected items outside this filter. Removal includes these items.", size)
			remove := visibleBox(t, tt, "Remove 4000 items (2.0 MB)", size)
			if note.Y+note.H/2 < remove.Y+remove.H/2-4 || note.Y+note.H/2 > remove.Y+remove.H/2+4 || note.H > 18 || note.X+note.W > remove.X-8 {
				t.Fatalf("filtered footer is not one row: note=%+v remove=%+v", note, remove)
			}
			snapshot(t, tt, fmt.Sprintf("large-search-%dx%d", size[0], size[1]))
			if err := leftoverAction(tt, "Deselect shown"); err != nil {
				t.Fatal(err)
			}
			if m.leftovers[4999].Selected || !m.leftovers[4998].Selected {
				t.Fatal("filtered deselection changed a hidden selection")
			}
			if err := leftoverAction(tt, "Select shown"); err != nil {
				t.Fatal(err)
			}
			if !m.leftovers[4999].Selected {
				t.Fatal("filtered selection missed its match")
			}
			if err := leftoverAction(tt, "Possible matches only"); err != nil {
				t.Fatal(err)
			}
			if len(m.matched) != 0 || !tt.HasText("No matching leftovers") {
				t.Fatal("search and possible-match filter did not combine")
			}
			visibleBox(t, tt, "Clear filters", size)
			snapshot(t, tt, fmt.Sprintf("large-no-matches-%dx%d", size[0], size[1]))
			if err := tt.Click("Clear filters"); err != nil {
				t.Fatal(err)
			}
			if len(m.matched) != 5000 || m.leftoverQuery != "" || m.leftoverMaybe || !m.folded["registry/Settings named after the app"] {
				t.Fatal("clearing filters did not restore the original tree")
			}
			if err := leftoverAction(tt, "Possible matches only"); err != nil {
				t.Fatal(err)
			}
			if len(m.matched) != 1000 {
				t.Fatalf("possible-only matches = %d", len(m.matched))
			}
			if err := leftoverAction(tt, "Select shown"); err != nil {
				t.Fatal(err)
			}
			for _, l := range m.leftovers {
				if !l.Selected {
					t.Fatal("Select shown missed an offscreen match")
				}
			}
			if err := leftoverAction(tt, "Deselect shown"); err != nil {
				t.Fatal(err)
			}
			for _, l := range m.leftovers {
				if l.Selected != l.Sure {
					t.Fatal("Deselect shown changed hidden sure matches")
				}
			}
			snapshot(t, tt, fmt.Sprintf("large-possible-%dx%d", size[0], size[1]))
		})
	}
}

func TestLeftoverFilteredGroup(t *testing.T) {
	m := largeLeftoverModel(5000, true)
	tt := ui.NewTester(m.view, 1060, 700)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyF)
	tt.Type("entry-0499")
	if len(m.matched) != 10 {
		t.Fatalf("search matches = %d", len(m.matched))
	}
	if err := tt.Click("Choose all of Registry"); err != nil {
		t.Fatal(err)
	}
	for i, l := range m.leftovers {
		want := l.Sure || i >= 4990
		if l.Selected != want {
			t.Fatalf("filtered group selection changed item %d to %v", i, l.Selected)
		}
	}
	// A live size update must refresh totals without discarding the filter,
	// selection, folds, or scroll position. Removal stays disabled while sizing.
	m.sizing = true
	m.leftovers[4999].Size = 1 << 20
	tt.Frame()
	if err := tt.Click("Remove 4002 items (1.0 MB)"); err != nil {
		t.Fatal(err)
	}
	if m.phase != phaseResults || len(m.matched) != 10 || !tt.HasText("1.0 MB") {
		t.Fatal("measurement allowed removal or lost the current results")
	}
	for _, query := range []string{`HKEY_CURRENT_USER\Software\VideoLAN`, "SETTINGS NAMED AFTER THE APP"} {
		m.leftoverQuery = query
		tt.Frame()
		if len(m.matched) != 5000 {
			t.Fatalf("full path/reason query %q found %d entries", query, len(m.matched))
		}
	}
}

func TestLeftoverKeyboard(t *testing.T) {
	m := largeLeftoverModel(5000, true)
	tt := ui.NewTester(m.view, 1060, 700)
	tt.Frame()
	m.foldAll(false)
	tt.Frame()
	m.rowList.Handle.Focus()
	tt.Frame()
	tt.Key(0, ui.KeyEnd)
	visibleBox(t, tt, "entry-04999", [2]int{1060, 700})
	if err := tt.Click("Choose " + m.leftovers[4999].Display()); err != nil {
		t.Fatal(err)
	}
	if m.leftovers[4999].Selected {
		t.Fatal("click did not toggle the final checkbox")
	}
	tt.Key(0, ui.KeySpace)
	if !m.leftovers[4999].Selected {
		t.Fatal("Space did not toggle the focused checkbox")
	}
	tt.Key(0, ui.KeyHome)
	visibleBox(t, tt, "entry-00000", [2]int{1060, 700})
	first, _ := m.rowList.Visible()
	tt.Key(0, ui.KeyPageDown)
	next, _ := m.rowList.Visible()
	if next <= first {
		t.Fatal("Page Down did not scroll the results")
	}
	tt.Key(0, ui.KeyPageUp)
	back, _ := m.rowList.Visible()
	if back >= next {
		t.Fatal("Page Up did not scroll back")
	}
	tt.Key(ui.Cmd, ui.KeyF)
	if !tt.Focused("Search leftovers") {
		t.Fatal("Ctrl+F did not return to search from the list")
	}
}

func TestLeftoverScaleBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 100, 50000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			m := largeLeftoverModel(n, true)
			tt := ui.NewTester(m.view, 720, 480)
			tt.Frame()
			if n == 0 {
				if !tt.HasText("No leftovers") || !tt.HasText("Done") {
					t.Fatal("missing zero-results state")
				}
				return
			}
			m.foldAll(false)
			m.rowList.ScrollToEnd()
			tt.Frame()
			visibleBox(t, tt, fmt.Sprintf("entry-%05d", n-1), [2]int{720, 480})
			assertVirtualLeftovers(t, tt, m)
		})
	}
	// Thousands of singleton groups cannot rely on folding to stay responsive.
	m := largeLeftoverModel(5000, true)
	for i := range m.leftovers {
		m.leftovers[i].Reason = fmt.Sprintf("Reason %d", i)
	}
	tt := ui.NewTester(m.view, 720, 480)
	tt.Frame()
	assertVirtualLeftovers(t, tt, m)
	m.rowList.ScrollToEnd()
	tt.Frame()
	visibleBox(t, tt, "entry-04999", [2]int{720, 480})
	if err := leftoverAction(tt, "Collapse all"); err != nil {
		t.Fatal(err)
	}
	if len(m.rows) != 1 {
		t.Fatalf("Collapse all left %d rows in a singleton-group section", len(m.rows))
	}
	// Identical labels must still have unique stable keys for keyboard focus.
	m.leftovers = []Leftover{m.leftovers[0], m.leftovers[0]}
	tt.Frame()
	keys := map[any]bool{}
	for row := range m.rows {
		key := m.rowList.Key(row)
		if keys[key] {
			t.Fatalf("duplicate row key %v", key)
		}
		keys[key] = true
	}
}
