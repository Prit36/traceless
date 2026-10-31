package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows/registry"
)

// Exercise the layout boundaries with content that cannot rely on short names.
// All interactions run in the headless view; no real app or file is removed.
func TestViewLayoutBoundaries(t *testing.T) {
	for _, size := range [][2]int{{720, 480}, {760, 520}, {900, 600}, {999, 600}, {1000, 600}, {1060, 700}, {1440, 900}} {
		for _, dark := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx%d/dark=%v", size[0], size[1], dark), func(t *testing.T) {
				m := sampleModel()
				m.chosen = 2 // The longer Remove leftovers command.
				tt := ui.NewTester(m.view, size[0], size[1])
				tt.SetDark(dark)
				tt.Frame()
				search := visibleBox(t, tt, "Search apps", size)
				visibleBox(t, tt, "Refresh", size)
				visibleBox(t, tt, "Remove leftovers", size)
				openLabel := "Open location"
				if size[0] < compactAt {
					openLabel = "Open the install folder"
				}
				open := visibleBox(t, tt, openLabel, size)
				if open.X+open.W > search.X {
					t.Fatal("commands overlap search")
				}
				snapshot(t, tt, fmt.Sprintf("main-%dx%d-dark-%v", size[0], size[1], dark))

				m.target = m.apps[3]
				m.leftovers = layoutLeftovers()
				m.phase = phaseResults
				tt.Frame()
				all := visibleBox(t, tt, "Select all", size)
				none := visibleBox(t, tt, "Select none", size)
				skip := visibleBox(t, tt, "Skip", size)
				remove := visibleBox(t, tt, "Remove 2 items (1.0 MB)", size)
				if all.X+all.W > none.X || remove.X+remove.W > skip.X || all.Y+all.H >= remove.Y {
					t.Fatal("dialog actions overlap")
				}
				// Header and footer stay compact, and no initial row is cut off.
				list := visibleBox(t, tt, "Leftovers", size)
				if list.Y-(all.Y+all.H) > 16 || remove.H > controlHeight+1 {
					t.Fatalf("header or footer has excessive vertical space: list=%+v all=%+v remove=%+v", list, all, remove)
				}
				for _, l := range m.leftovers {
					name, parent := shortPath(&l)
					if r, ok := tt.Find(name); ok && r.Y >= list.Y+tableHeader && r.Y < list.Y+list.H {
						why, _ := splitReason(l.Reason)
						meta, ok := tt.Find(why + " · " + parent)
						if !ok || meta.Y+meta.H > list.Y+list.H+1 {
							t.Fatal("initial table clips a row")
						}
					}
				}
				if err := tt.Click("Select none"); err != nil {
					t.Fatal(err)
				}
				for _, l := range m.leftovers {
					if l.Selected {
						t.Fatal("Select none missed an item")
					}
				}
				if err := tt.Click("Select all"); err != nil {
					t.Fatal(err)
				}
				for _, l := range m.leftovers {
					if !l.Selected {
						t.Fatal("Select all missed an item")
					}
				}

				// Scrolling must reach the final row above the persistent footer.
				list = visibleBox(t, tt, "Leftovers", size)
				tt.Scroll(list.X+list.W/2, list.Y+list.H/2, 0, 10000)
				last := visibleBox(t, tt, "Final registry entry", size)
				remove = visibleBox(t, tt, "Remove 4 items (2.0 MB)", size)
				if last.Y+last.H > remove.Y-16 {
					t.Fatal("last row is obscured by the footer")
				}
				snapshot(t, tt, fmt.Sprintf("layout-%dx%d-dark-%v", size[0], size[1], dark))
			})
		}
	}
}

func visibleBox(t *testing.T, tt *ui.Tester, label string, size [2]int) ui.Rect {
	t.Helper()
	r, ok := tt.Find(label)
	if !ok || r.W <= 0 || r.H <= 0 || r.X < 0 || r.Y < 0 || r.X+r.W > float32(size[0])+1 || r.Y+r.H > float32(size[1])+1 {
		t.Fatalf("%q is outside %v: %+v (found %v)", label, size, r, ok)
	}
	return r
}

func layoutLeftovers() []Leftover {
	return []Leftover{
		{Kind: KindFolder, Path: `C:\Program Files\` + strings.Repeat("A long application name ", 5), Reason: "Publisher's folder, holding only the app's files", Sure: true, Selected: true, Size: 1 << 20},
		{Kind: KindFolder, Path: `C:\Users\Ada\Documents\Recordings`, Reason: "Folder named after the app — may hold your own files", Size: 1 << 20},
		{Kind: KindKey, Hive: registry.CURRENT_USER, Path: `Software\VideoLAN`, Reason: "Publisher's key, holding only the app's settings", Sure: true, Selected: true},
		{Kind: KindValue, Hive: registry.CURRENT_USER, Path: `Software\Microsoft\Windows\CurrentVersion\Run`, Value: "Final registry entry", Reason: "Starts with Windows", Sure: true},
	}
}

// Capture states outside the main uninstall flow, including scrollable error
// details and minimum-size dialogs, for visual review via SNAPSHOTS.
func TestViewAdditionalStates(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*model)
	}{
		{"loading", func(m *model) { m.apps, m.shown, m.loading = nil, nil, true }},
		{"empty-search", func(m *model) { m.query = strings.Repeat("No matching application ", 5); m.filter() }},
		{"running", func(m *model) { m.phase = phaseRunning }},
		{"scanning", func(m *model) { m.phase = phaseScanning }},
		{"cleaning", func(m *model) { m.phase = phaseCleaning }},
		{"measuring", func(m *model) { m.phase, m.sizing, m.leftovers = phaseResults, true, layoutLeftovers() }},
		{"no-leftovers", func(m *model) { m.phase = phaseResults }},
		{"long-title", func(m *model) {
			m.target.Name = strings.Repeat("A long application name ", 5)
			m.phase, m.leftovers = phaseResults, layoutLeftovers()
		}},
		{"confirm-small", func(m *model) { m.phase = phaseConfirm }},
		{"success", func(m *model) { m.phase = phaseReport; m.result = CleanResult{Removed: 4, Freed: 2 << 20} }},
		{"uninstall-error", func(m *model) { m.phase = phaseFinished; m.runErr = errors.New("The uninstaller could not be opened") }},
		{"long-report", func(m *model) {
			m.phase = phaseReport
			for range 12 {
				m.result.Failed = append(m.result.Failed, Failure{layoutLeftovers()[0], errors.New("The process cannot access the file because it is being used by another process.")})
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.target, m.chosen = m.apps[3], 3
			tc.setup(m)
			tt := ui.NewTester(m.view, 720, 480)
			snapshot(t, tt, "state-"+tc.name)
			if m.phase == phaseReport {
				visibleBox(t, tt, "Done", [2]int{720, 480})
			}
		})
	}
}
