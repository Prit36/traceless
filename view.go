package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows"
)

// phase is where removing an app stands: each shows a dialog.
type phase int

const (
	phaseNone     phase = iota
	phaseConfirm        // asks before running the uninstaller
	phaseRunning        // the uninstaller runs
	phaseFinished       // it is over: look for leftovers?
	phaseScanning       // looking for them
	phaseResults        // what was found, to choose from
	phaseCleaning       // removing what was chosen
	phaseReport         // what that did
)

const (
	iconSize      = 20 // DIPs, in the table
	largeIcon     = 32 // in the details pane and the dialogs
	compactAt     = 1000
	contentInset  = 24
	tableInset    = contentInset - 10 // Table cells already include 10 DIPs of padding.
	controlHeight = 32
)

// A view of the apps, in the navigation pane.
type navItem struct {
	id, label string
	glyph     rune
	match     func(a *App) bool // nil for all
	tip       string
}

var navViews = []navItem{
	{"all", "All apps", glyphAllApps, nil, ""},
	{"recent", "Recently installed", glyphRecent, func(a *App) bool {
		return !a.InstallDate.IsZero() && time.Since(a.InstallDate) < 30*24*time.Hour
	}, "Installed in the last 30 days"},
	{"large", "Large apps", glyphDrive, func(a *App) bool { return a.SizeKB >= 500<<10 }, "500 MB or more"},
	{"broken", "Uninstaller missing", glyphWarning, func(a *App) bool { return a.Broken },
		"Apps whose uninstaller is gone: only their leftovers can be removed"},
}

func navFind(id string) navItem {
	for _, n := range navViews {
		if n.id == id {
			return n
		}
	}
	return navViews[0]
}

// The table shows what decides whether an app goes: what it is, who made
// it, how much room it takes and how long it has been there. Its version,
// and where it is, are in the details pane.
var appColumns = []ui.TableColumn{
	{Title: "Name", MinWidth: 200, Sortable: true},
	{Title: "Publisher", Width: 156, Sortable: true},
	{Title: "Size", Width: 140, Align: ui.End, Sortable: true},
	{Title: "Installed", Width: 112, Sortable: true},
}

// A leftover's row gives the room to its path, which is what tells whether
// it is the app's: its name, and under it why it was found and where it is.
var leftoverColumns = []ui.TableColumn{
	{Title: "", ID: "check", Width: 40, Fixed: true, Align: ui.Center},
	{Title: "Item", MinWidth: 240},
	{Title: "Size", Width: 90, Align: ui.End},
}

const (
	leftoverRow   = 52 // DIPs, including the table cell's 12-DIP vertical padding.
	headerRow     = 32 // a section's header
	tableHeader   = 32
	resultsHeader = 48
)

// model is the app's state, read and changed on the main thread; work that
// takes time runs on goroutines, which hand their results back through
// Window.Update.
type model struct {
	win      *mygo.Window
	elevated bool
	pal      palette
	logo     *ui.Bitmap

	apps        []*App // all of them, by name
	shown       []*App // those the view and the search find, sorted
	counts      map[string]int
	totalKB     uint64
	maxKB       uint64 // the largest app shown, the full length of the size bars
	loading     bool
	nav         string
	query       string
	sort        ui.SortOrder
	list        ui.ListState
	chosen      int
	search      ui.Handle
	indexedApps []*App
	countsAt    time.Time

	icons      map[string]*ui.Bitmap // by icon path; nil where there is none
	iconUsed   map[string]uint64
	iconClock  uint64
	pending    map[string]bool
	iconReq    chan string
	iconPx     int
	stop       chan struct{}
	stopOnce   sync.Once
	reloadID   uint64
	scanID     uint64
	scanCancel context.CancelFunc
	sizing     bool

	phase                    phase
	open                     bool
	target                   *App
	quiet                    bool
	run                      *Uninstall
	runErr                   error
	gone                     bool // the app's entry went away as its uninstaller ran
	leftovers                []Leftover
	rows                     []int // the leftovers table's rows: indexes, or a section's header
	rowsItems                []Leftover
	fileCount, registryCount int
	rowList                  ui.ListState
	result                   CleanResult
	toast                    string
}

// The headers of the leftovers table's sections.
const (
	rowFiles    = -1
	rowRegistry = -2
)

func newModel(elevated bool) *model {
	return &model{
		elevated: elevated,
		loading:  true,
		nav:      "all",
		sort:     ui.SortOrder{Column: "Name"},
		counts:   make(map[string]int),
		icons:    make(map[string]*ui.Bitmap),
		iconUsed: make(map[string]uint64),
		pending:  make(map[string]bool),
		iconReq:  make(chan string, 32),
		iconPx:   largeIcon,
		logo:     logo(16),
		stop:     make(chan struct{}),
	}
}

// start reads the apps once the window is there.
func (m *model) start() {
	scale := 1.0
	for _, d := range mygo.Screen.Displays() {
		scale = max(scale, d.ScaleFactor)
	}
	m.iconPx = int(largeIcon*scale + 0.5)
	if scale != 1 {
		m.logo = logo(int(16*scale + 0.5))
	}
	m.win.OnClosed(m.shutdown)
	go m.iconWorker()
	m.reload()
}

func (m *model) reload() {
	m.reloadID++
	id := m.reloadID
	m.loading = true
	go func() {
		apps := LoadApps()
		m.win.Update(func() {
			if id != m.reloadID {
				return
			}
			m.apps, m.loading = apps, false
			m.filter()
		})
	}()
}

// filter shows the apps of the view chosen whose name or publisher holds
// the query, sorted, keeping the app chosen.
func (m *model) filter() {
	var id string
	if a := m.selected(); a != nil {
		id = a.ID()
	}
	view := navFind(m.nav)
	q := strings.ToLower(strings.TrimSpace(m.query))
	if !sameApps(m.apps, m.indexedApps) || time.Since(m.countsAt) > time.Minute {
		clear(m.counts)
		for _, a := range m.apps {
			for _, n := range navViews {
				if n.match == nil || n.match(a) {
					m.counts[n.id]++
				}
			}
		}
		m.indexedApps, m.countsAt = m.apps, time.Now()
	}
	previous := m.shown
	m.shown, m.totalKB, m.maxKB = m.shown[:0], 0, 0
	for _, a := range m.apps {
		if (view.match == nil || view.match(a)) &&
			(q == "" || strings.Contains(a.lowerName, q) || strings.Contains(a.lowerPublisher, q)) {
			m.shown = append(m.shown, a)
			m.totalKB += a.SizeKB
			m.maxKB = max(m.maxKB, a.SizeKB)
		}
	}
	if len(previous) > len(m.shown) {
		clear(previous[len(m.shown):])
	}
	var by func(a, b *App) int
	switch m.sort.Column {
	case "Publisher":
		by = func(a, b *App) int { return cmp.Compare(a.lowerPublisher, b.lowerPublisher) }
	case "Size":
		by = func(a, b *App) int { return cmp.Compare(a.SizeKB, b.SizeKB) }
	case "Installed":
		by = func(a, b *App) int { return a.InstallDate.Compare(b.InstallDate) }
	default: // the apps are read sorted by name
	}
	if by != nil || m.sort.Descending {
		slices.SortStableFunc(m.shown, func(a, b *App) int {
			r := 0
			if by != nil {
				r = by(a, b)
			} else {
				r = cmp.Compare(a.lowerName, b.lowerName)
			}
			if m.sort.Descending {
				return -r
			}
			return r
		})
	}
	m.chosen = 0
	for i, a := range m.shown {
		if a.ID() == id {
			m.chosen = i
			break
		}
	}
	if len(m.shown) > 0 {
		m.list.ScrollIntoView(m.chosen)
	}
}

func sameApps(a, b []*App) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func (m *model) selected() *App {
	if m.chosen < 0 || m.chosen >= len(m.shown) {
		return nil
	}
	return m.shown[m.chosen]
}

// icon returns an app's icon once it is loaded, asking for it the first
// time a row shows it, so that only the icons in view are ever drawn.
func (m *model) icon(a *App) *ui.Bitmap {
	p := a.DisplayIcon
	if p == "" {
		return nil
	}
	if b, ok := m.icons[p]; ok {
		m.iconClock++
		m.iconUsed[p] = m.iconClock
		return b
	}
	if !m.pending[p] {
		select {
		case m.iconReq <- p:
			m.pending[p] = true
		default: // asked again as the row shows next
		}
	}
	return nil
}

func (m *model) iconWorker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err := initIconApartment()
	if err == nil {
		defer windows.CoUninitialize()
	}
	type loadedIcon struct {
		path   string
		bitmap *ui.Bitmap
	}
	for {
		var p string
		select {
		case <-m.stop:
			return
		case p = <-m.iconReq:
		}
		batch := make([]loadedIcon, 0, 8)
		started := time.Now()
	loadBatch:
		for {
			var b *ui.Bitmap
			if err == nil {
				b, _ = loadFileIcon(p, m.iconPx)
			}
			batch = append(batch, loadedIcon{p, b})
			if len(batch) == cap(batch) || time.Since(started) >= 12*time.Millisecond {
				break
			}
			select {
			case <-m.stop:
				return
			case p = <-m.iconReq:
			default:
				break loadBatch
			}
		}
		m.win.Update(func() {
			for _, icon := range batch {
				m.cacheIcon(icon.path, icon.bitmap)
				delete(m.pending, icon.path)
			}
		})
	}
}

// Bound both bitmap pixels (including high-DPI icons) and failed lookups. The
// renderer releases textures after their bitmaps are no longer referenced.
func (m *model) cacheIcon(path string, b *ui.Bitmap) {
	limit := min(128, max(16, (2<<20)/max(1, m.iconPx*m.iconPx*4)))
	if _, ok := m.icons[path]; !ok && len(m.icons) >= limit {
		var oldest string
		age := ^uint64(0)
		for p, used := range m.iconUsed {
			if used < age {
				oldest, age = p, used
			}
		}
		delete(m.icons, oldest)
		delete(m.iconUsed, oldest)
	}
	m.iconClock++
	m.icons[path], m.iconUsed[path] = b, m.iconClock
}

func (m *model) shutdown() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.cancelScan()
}

func (m *model) cancelScan() {
	m.scanID++
	if m.scanCancel != nil {
		m.scanCancel()
		m.scanCancel = nil
	}
	m.sizing = false
}

func (m *model) view(c *ui.Context) {
	t, p := fluent(c)
	m.pal = p
	m.setTheme(c, t)
	if m.toast != "" {
		c.Toast(m.toast)
		m.toast = ""
	}
	if m.phase == phaseNone {
		if c.Shortcut(ui.Cmd, ui.KeyF) {
			m.search.Focus()
		}
		if c.Shortcut(0, ui.KeyF5) && !m.loading {
			m.reload()
		}
		if c.Shortcut(0, ui.KeyDelete) {
			if a := m.selected(); a != nil {
				m.ask(a)
			}
		}
	}
	w, _ := c.Size()
	compact := w < compactAt
	ui.Column(c).Fill().Children(func() {
		m.titleBar(c, t)
		ui.Row(c).Grow(1).Shrink(1).AlignItems(ui.Stretch).Children(func() {
			m.navPane(c, t, compact)
			ui.Column(c).Grow(1).Shrink(1).Background(t.Background).
				BorderWidth(1, 0, 0, 1).BorderColor(p.stroke).Radius(8, 0, 0, 0).Clip().Children(func() {
				m.commands(c, t, compact)
				m.appTable(c, t)
				m.details(c, t)
			})
		})
	})
	m.dialog(c, t)
}

// setTheme sets the theme of what is built next, over Mica where the window
// shows it.
func (m *model) setTheme(c *ui.Context, t *ui.Theme) {
	c.SetTheme(t)
	if c.Vibrancy() {
		c.Root().Background(ui.Transparent)
	} else {
		c.Root().Background(m.pal.window)
	}
}

// titleBar draws the window's title bar, under its buttons, as Windows 11
// apps do: the app's icon and name, over Mica.
func (m *model) titleBar(c *ui.Context, t *ui.Theme) {
	bar := c.TitleBar()
	ui.Row(c).Height(max(bar.Height, 40)).Padding(0, bar.Right+12, 0, bar.Left+16).Gap(12).
		AlignItems(ui.Center).DragWindow().Shrink(0).Children(func() {
		if m.logo != nil {
			ui.Image(c, m.logo).Size(16, 16)
		}
		ui.Text(c, "Traceless").FontSize(12).SingleLine()
	})
}

// navPane chooses which apps the table shows, as Windows 11's navigation
// pane, down to its icons when the window is narrow.
func (m *model) navPane(c *ui.Context, t *ui.Theme, compact bool) {
	width := float32(240)
	if compact {
		width = 64
	}
	ui.Column(c).Width(width).Shrink(0).Padding(8, 8, 12, 8).Gap(4).Children(func() {
		for _, n := range navViews {
			m.navButton(c, t, n, compact)
		}
		ui.Box(c).Grow(1)
		m.adminButton(c, t, compact)
	})
}

func (m *model) navButton(c *ui.Context, t *ui.Theme, n navItem, compact bool) {
	on := m.nav == n.id
	item := ui.Row(c.Key("nav-"+n.id)).Height(36).Padding(0, 12).Gap(12).Radius(4).AlignItems(ui.Center).
		Focusable().Role(ui.RoleButton).Label(n.label)
	switch {
	case on:
		item.Background(m.pal.chosen)
	case item.Pressed():
		item.Background(m.pal.subtle).Opacity(0.8)
	case item.Hovered():
		item.Background(m.pal.subtle)
	}
	switch {
	case compact && n.tip != "":
		item.Tooltip(n.label + ": " + n.tip)
	case compact:
		item.Tooltip(n.label)
	case n.tip != "":
		item.Tooltip(n.tip)
	}
	if item.Clicked() && !on {
		m.nav = n.id
		m.filter()
	}
	// Apps whose uninstaller is gone are the ones to see to: their count
	// is a badge, where the others' are plain.
	alert := n.id == "broken" && m.counts[n.id] > 0 && !m.loading
	item.Children(func() {
		if on {
			ui.Box(c).Absolute().Left(0).Top(10).Size(3, 16).Radius(1.5).Background(t.Accent)
		}
		ui.Box(c).Size(20, 20).Shrink(0).Center().Children(func() {
			g := glyph(c, n.glyph)
			if alert {
				g.TextColor(t.Warning)
			}
		})
		switch {
		case compact:
		case alert:
			ui.Text(c, n.label).Grow(1).Shrink(1).SingleLine()
			ui.Text(c, fmt.Sprint(m.counts[n.id])).FontSize(11).FontWeight(600).SingleLine().
				Padding(2, 6).MinWidth(20).TextAlign(ui.Center).Radius(10).Background(m.pal.warning)
		default:
			ui.Text(c, n.label).Grow(1).Shrink(1).SingleLine()
			if !m.loading {
				ui.Text(c, fmt.Sprint(m.counts[n.id])).FontSize(12).FontFeatures("tnum").MinWidth(20).TextAlign(ui.End).TextColor(t.TextMuted)
			}
		}
	})
}

// adminButton says whether the app runs as administrator, and restarts it
// as one when it does not.
func (m *model) adminButton(c *ui.Context, t *ui.Theme, compact bool) {
	label := "Running as administrator"
	if !m.elevated {
		label = "Restart as administrator"
	}
	item := ui.Row(c.Key("admin")).MinHeight(40).Padding(6, 12).Gap(12).Radius(4).AlignItems(ui.Center).Label(label)
	if compact {
		item.Tooltip(label)
	}
	if !m.elevated {
		item.Focusable().Role(ui.RoleButton)
		if !compact {
			item.Tooltip("Apps installed for everyone, and their leftovers, need administrator rights to come off")
		}
		if item.Hovered() {
			item.Background(m.pal.subtle)
		}
		if item.Clicked() {
			m.restartElevated(c)
		}
	}
	item.Children(func() {
		ui.Box(c).Size(20, 20).Shrink(0).Center().Children(func() {
			g := glyph(c, glyphShield)
			if !m.elevated {
				g.TextColor(t.Warning)
			}
		})
		if compact {
			return
		}
		ui.Column(c).Grow(1).Shrink(1).Gap(4).Children(func() {
			ui.Text(c, label).SingleLine()
			if !m.elevated {
				ui.Text(c, "Required for some apps").FontSize(12).TextColor(t.TextMuted).SingleLine()
			}
		})
	})
}

// commands is the command bar, as Explorer's: what can be done with the app
// chosen, then how many apps the view holds and the room they take, the
// search and Refresh. The view's name is in the navigation pane already.
func (m *model) commands(c *ui.Context, t *ui.Theme, compact bool) {
	a := m.selected()
	ui.Row(c).Padding(8, 16).Gap(12).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Toolbar(c, func() {
			if a != nil && a.Broken {
				if command(c, "uninstall", glyphBroom, "Remove leftovers", "Its uninstaller is missing: remove what's left of it (Del)", false) {
					m.ask(a)
				}
			} else if command(c, "uninstall", glyphDelete, "Uninstall", "Uninstall (Del)", a == nil) {
				m.ask(a)
			}
			openLabel := "Open location"
			if compact {
				openLabel = ""
			}
			if command(c, "open", glyphOpen, openLabel, "Open the install folder", a == nil || !a.dirExists) {
				mygo.Shell.OpenPath(a.Dir)
			}
		}).Label("Commands").Grow(1).Shrink(1)
		summary := "Reading the installed apps…"
		if !m.loading {
			summary = plural(len(m.shown), "app")
			if m.totalKB > 0 {
				summary += " · " + formatBytes(int64(m.totalKB)*1024)
			}
		}
		if !compact { // the commands come first
			ui.Text(c, summary).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(0)
		}
		if ui.SearchField(c.Key("search"), &m.query).Bind(&m.search).Placeholder("Search apps").
			Label("Search apps").Tooltip("Search by name or publisher (Ctrl+F)").Width(240).MinWidth(160).Height(controlHeight).Shrink(1).Changed() {
			m.filter()
		}
		ui.Toolbar(c, func() {
			if ui.Button(c.Key("refresh"), "").Label("Refresh").Tooltip("Refresh (F5)").Size(controlHeight, controlHeight).Padding(8).Disabled(m.loading).
				Children(func() { glyph(c, glyphRefresh) }).Clicked() {
				m.reload()
			}
		}).Label("View").Shrink(0)
	})
}

func command(c *ui.Context, key string, g rune, label, tip string, disabled bool) bool {
	accessibleLabel := label
	if accessibleLabel == "" {
		accessibleLabel = tip
	}
	b := ui.Button(c.Key(key), "").Label(accessibleLabel).Height(controlHeight).Gap(8).Padding(6, 8).Disabled(disabled).Children(func() {
		glyph(c, g)
		if label != "" {
			ui.Text(c, label).FixedLineHeight(18).SingleLine()
		}
	})
	if tip != "" {
		b.Tooltip(tip)
	}
	return b.Clicked()
}

func (m *model) appTable(c *ui.Context, t *ui.Theme) {
	if len(m.shown) == 0 {
		ui.Column(c).Grow(1).Shrink(1).Padding(contentInset).Center().Gap(16).BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke).Children(func() {
			switch {
			case m.loading:
				ui.Spinner(c).Size(32, 32).Label("Loading")
			case m.query != "":
				glyph(c, '').FontSize(32).TextColor(t.TextMuted)
				ui.Text(c, "No apps match “"+m.query+"”").TextColor(t.TextMuted).TextAlign(ui.Center)
			default:
				glyph(c, glyphCompleted).FontSize(32).TextColor(t.TextMuted)
				ui.Text(c, "Nothing here").TextColor(t.TextMuted)
			}
		})
		return
	}
	m.list.Selected = &m.chosen
	m.list.Sort = &m.sort
	m.list.Key = func(i int) any { return m.shown[i].ID() }
	m.list.Label = func(i int) string { return m.shown[i].Name }
	m.list.CellMenu = func(menu *ui.Menu, row, _ int) {
		a := m.shown[row]
		label := "Uninstall"
		if a.Broken {
			label = "Remove leftovers"
		}
		if menu.Item(label).Chosen() {
			m.chosen = row
			m.ask(a)
		}
		if menu.Item("Open location").Disabled(!a.dirExists).Chosen() {
			mygo.Shell.OpenPath(a.Dir)
		}
		if menu.Item("Copy name").Chosen() {
			mygo.Clipboard.WriteText(a.Name)
		}
	}
	// The chosen row is tinted, as in Explorer, not filled with the accent.
	c.SetTheme(rowsTheme(t, m.pal))
	table := ui.Table(c.Key("apps"), &m.list, appColumns, len(m.shown), func(row, col int) { m.cell(c, t, row, col) }).
		Grow(1).Shrink(1).Margin(0, tableInset).BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke).Label("Installed apps")
	m.setTheme(c, t)
	if table.Changed() { // sorted by another column
		m.filter()
	}
	if table.Submitted() {
		if a := m.selected(); a != nil {
			m.ask(a)
		}
	}
	// The table follows its choice by key, and chooses nothing when a
	// search hides it: choose the first app shown instead.
	if m.chosen < 0 {
		m.chosen = 0
		c.Invalidate()
	}
}

func (m *model) cell(c *ui.Context, t *ui.Theme, row, col int) {
	a := m.shown[row]
	muted := func(s string) { ui.Text(c, s).FontSize(13).TextColor(t.TextMuted).SingleLine().Shrink(1).Tooltip(s) }
	switch col {
	case 0:
		ui.Row(c).MinHeight(28).Gap(12).AlignItems(ui.Center).Shrink(1).Grow(1).Tooltip(a.Name).Children(func() {
			appIcon(c, t, m.icon(a), iconSize)
			ui.Text(c, a.Name).SingleLine().Shrink(1)
			if a.Broken {
				w, _ := c.Size()
				if w < 1000 {
					glyph(c, glyphWarning).TextColor(t.Warning).Tooltip("Uninstaller missing")
				} else {
					m.tag(c, t, "Uninstaller missing", m.pal.warning)
				}
			}
		})
	case 1:
		muted(a.Publisher)
	case 2:
		m.sizeCell(c, t, a)
	case 3:
		muted(a.dateText)
	}
}

// sizeCell shows the room an app takes, with a bar against the largest
// shown, so that what takes the most stands out as the list is scanned.
func (m *model) sizeCell(c *ui.Context, t *ui.Theme, a *App) {
	if a.SizeKB == 0 {
		return
	}
	share := float32(a.SizeKB) / float32(max(m.maxKB, 1))
	ui.Row(c).Gap(12).AlignItems(ui.Center).Grow(1).Shrink(1).Children(func() {
		ui.Box(c).Width(36).Height(4).Radius(2).Shrink(0).Background(m.pal.tag).Children(func() {
			ui.Box(c).Width(max(2, 36*share)).Height(4).Radius(2).Background(t.Accent.Alpha(0.35 + 0.65*share))
		})
		ui.Text(c, a.sizeText).FontSize(13).FontFeatures("tnum").SingleLine().
			Grow(1).TextAlign(ui.End)
	})
}

func appIcon(c *ui.Context, t *ui.Theme, b *ui.Bitmap, size float32) {
	if b != nil {
		ui.Image(c, b).Size(size, size).Shrink(0)
		return
	}
	// Windows' icon of an app without one.
	ui.Box(c).Size(size, size).Shrink(0).Center().Children(func() {
		glyph(c, '').FontSize(size * 0.85).TextColor(t.TextMuted)
	})
}

// tag is a small label of a row, as "Possible match".
func (m *model) tag(c *ui.Context, t *ui.Theme, label string, bg ui.Color) ui.Element {
	return ui.Text(c, label).FontSize(11).FixedLineHeight(14).SingleLine().Padding(2, 6).Radius(4).Background(bg).Shrink(0)
}

// details tells about the app chosen what its row does not: its version,
// who it is installed for, how it uninstalls and where it is.
func (m *model) details(c *ui.Context, t *ui.Theme) {
	a := m.selected()
	if a == nil {
		return
	}
	ui.Row(c).MinHeight(64).Padding(10, contentInset).Gap(16).AlignItems(ui.Center).Shrink(0).
		BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke).Children(func() {
		appIcon(c, t, m.icon(a), largeIcon)
		ui.Column(c).Grow(1).Shrink(1).Gap(4).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, a.Name).FontWeight(600).SingleLine().Shrink(1).Tooltip(a.Name)
				if a.Version != "" && !strings.Contains(a.Name, a.Version) {
					ui.Text(c, a.Version).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(0)
				}
			})
			facts := "Installed for everyone"
			if !a.AllUsers {
				facts = "Installed only for you"
			}
			if !a.Broken && a.CanQuiet() {
				facts += " · Can uninstall quietly"
			}
			ui.Text(c, facts).FontSize(12).TextColor(t.TextMuted).SingleLine().Shrink(1)
			switch {
			case a.Broken:
				ui.Text(c, "Its uninstaller is missing: only its leftovers can be removed").FontSize(12).
					TextColor(t.Warning).SingleLine().Shrink(1)
			case a.dirExists:
				if hyperlink(c, "dir", a.Dir).Tooltip("Open in File Explorer").Shrink(1).Clicked() {
					mygo.Shell.OpenPath(a.Dir)
				}
			}
		})
	})
}

func hyperlink(c *ui.Context, key, label string) ui.Element {
	return ui.Text(c.Key(key), label).FontSize(12).TextColor(c.Theme().Accent).SingleLine().
		Cursor(ui.CursorPointer).Focusable().Role(ui.RoleLink).Label(label)
}

// restartElevated starts the app again as administrator, and quits this one
// once the user agrees.
func (m *model) restartElevated(c *ui.Context) {
	if exe, err := os.Executable(); err == nil && relaunchElevated(exe) == nil {
		mygo.App.Quit()
	} else {
		c.Toast("Administrator rights weren't granted")
	}
}

// ask starts removing an app, asking first.
func (m *model) ask(a *App) {
	m.cancelScan()
	m.target, m.quiet, m.runErr, m.gone, m.leftovers = a, false, nil, false, nil
	m.result = CleanResult{}
	m.phase = phaseConfirm
}

func (m *model) uninstall() {
	a := m.target
	u, err := StartUninstall(a, m.quiet && a.CanQuiet())
	if err != nil {
		m.runErr, m.phase = err, phaseFinished
		return
	}
	m.run, m.phase = u, phaseRunning
	go func() {
		<-u.Done
		gone := !a.StillInstalled()
		m.win.Update(func() {
			if m.run == u {
				m.run, m.gone, m.phase = nil, gone, phaseFinished
			}
		})
	}()
}

// scan looks for the target's leftovers, with the apps read again: the
// uninstaller may have taken others with it.
func (m *model) scan() {
	m.cancelScan()
	ctx, cancel := context.WithCancel(context.Background())
	m.scanCancel = cancel
	id := m.scanID
	m.reloadID++ // an older refresh must not replace this scan's app list
	m.phase = phaseScanning
	a := m.target
	go func() {
		apps := LoadApps()
		if ctx.Err() != nil {
			return
		}
		items := NewScanner(a, apps).Discover(ctx)
		if ctx.Err() != nil {
			return
		}
		// Measurement owns items; the UI owns a separate slice so selection,
		// dismissal and size updates cannot race with the worker.
		visible := slices.Clone(items)
		m.win.Update(func() {
			if id != m.scanID || ctx.Err() != nil {
				return
			}
			m.apps, m.loading = apps, false
			m.filter()
			m.leftovers, m.phase, m.sizing = visible, phaseResults, len(items) > 0
		})
		var batch []sizeUpdate
		last := time.Now()
		publish := func() {
			if len(batch) == 0 {
				return
			}
			updates := batch
			batch = nil
			m.win.Update(func() {
				if id != m.scanID || ctx.Err() != nil {
					return
				}
				for _, u := range updates {
					m.leftovers[u.index].Size = u.size
				}
			})
			last = time.Now()
		}
		measureContext(ctx, items, func(u sizeUpdate) {
			batch = append(batch, u)
			if len(batch) >= 64 || time.Since(last) >= 75*time.Millisecond {
				publish()
			}
		})
		if ctx.Err() != nil {
			return
		}
		publish()
		m.win.Update(func() {
			if id == m.scanID {
				m.sizing = false
				m.scanCancel = nil
			}
		})
	}()
}

func (m *model) clean(items []Leftover, permanent bool) {
	m.cancelScan()
	m.phase = phaseCleaning
	name := m.target.Name
	before := m.result
	go func() {
		res := Clean(items, name, backupRoot(), permanent)
		m.win.Update(func() {
			if permanent { // a second try of the files that failed
				res.Removed += before.Removed
				res.Freed += before.Freed
				for _, f := range before.Failed {
					if f.Item.IsRegistry() {
						res.Failed = append(res.Failed, f)
					}
				}
				if res.BackupDir == "" {
					res.BackupDir = before.BackupDir
				}
			}
			m.result, m.phase = res, phaseReport
		})
	}()
}

func backupRoot() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Traceless", "Registry backups")
}

// close ends the dialogs, and reads the apps again when one may have gone.
func (m *model) close() {
	m.cancelScan()
	if m.phase >= phaseFinished {
		m.reload()
	}
	if m.phase == phaseReport && m.result.Removed > 0 {
		m.toast = fmt.Sprintf("Removed %s of %s", plural(m.result.Removed, "leftover"), m.target.Name)
	}
	m.phase, m.run = phaseNone, nil
	m.target, m.leftovers, m.rowsItems, m.rows = nil, nil, nil, nil
	m.result = CleanResult{}
	m.rowList = ui.ListState{}
}

// dialog shows what removing the target is at, over the window, as
// Windows 11's content dialogs: a title and what it says over the buttons'
// band.
func (m *model) dialog(c *ui.Context, t *ui.Theme) {
	m.open = m.phase != phaseNone
	if !m.open {
		return
	}
	// Lists of paths take what room the window has; messages are narrow.
	width := float32(520)
	switch {
	case m.phase == phaseResults && len(m.leftovers) > 0:
		width = 1000
	case m.phase == phaseReport && len(m.result.Failed) > 0:
		width = 820
	}
	ui.DialogBase(c.Key("dialog"), &m.open, func(backdrop, panel ui.Element) {
		backdrop.Background(m.pal.smoke)
		panel.Width(width).MaxWidthPercent(94).MaxHeightPercent(90).Radius(8).Clip().
			Background(t.Background).Border(1, m.pal.stroke).
			Shadow(0, 32, 64, 0, ui.RGBA(0, 0, 0, 0.19)).Shadow(0, 2, 21, 0, ui.RGBA(0, 0, 0, 0.15))
		switch m.phase {
		case phaseConfirm:
			m.confirmView(c, t)
		case phaseRunning:
			m.runningView(c, t)
		case phaseFinished:
			m.finishedView(c, t)
		case phaseScanning:
			m.busy(c, t, "Looking for leftovers", "Checking program folders, app data, shortcuts and the registry.")
		case phaseResults:
			m.resultsView(c, t)
		case phaseCleaning:
			m.busy(c, t, "Removing leftovers", "Moving files to the Recycle Bin and backing up registry entries.")
		case phaseReport:
			m.reportView(c, t)
		}
	})
	if !m.open { // Escape, or a click outside
		switch m.phase {
		case phaseConfirm, phaseFinished, phaseResults, phaseReport:
			m.close()
		}
	}
}

// body holds a dialog's title and content.
func (m *model) body(c *ui.Context, title string, fn func()) {
	ui.Scroll(c.Key("body")).Shrink(1).Children(func() {
		ui.Column(c).Padding(20, contentInset).Gap(12).Children(func() {
			ui.Text(c, title).Font("Segoe UI Variable Display, Segoe UI").FontSize(20).FontWeight(600)
			fn()
		})
	})
}

// footer is a dialog's band of buttons; in a narrow dialog they share its
// width, as Windows lays them out.
func (m *model) footer(c *ui.Context, fn func()) {
	ui.Row(c).Padding(8, contentInset).Gap(8).Justify(ui.End).AlignItems(ui.Center).Shrink(0).
		Background(m.pal.footer).BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke).Children(fn)
}

func wide(b ui.Element) ui.Element { return b.Grow(1).Basis(0).MinWidth(120).MinHeight(controlHeight) }

func (m *model) appLine(c *ui.Context, t *ui.Theme, a *App) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		appIcon(c, t, m.icon(a), largeIcon)
		ui.Column(c).Grow(1).Shrink(1).Gap(4).Children(func() {
			ui.Text(c, a.Name).FontWeight(600).SingleLine().Tooltip(a.Name)
			if line := byLine(a); line != "" {
				ui.Text(c, line).FontSize(12).TextColor(t.TextMuted).SingleLine()
			}
		})
	})
}

func (m *model) confirmView(c *ui.Context, t *ui.Theme) {
	a := m.target
	title := "Uninstall this app?"
	if a.Broken {
		title = "Remove what's left of this app?"
	}
	m.body(c, title, func() {
		m.appLine(c, t, a)
		if a.Broken {
			ui.Text(c, "Its uninstaller is gone, so the app can't remove itself. "+
				"Its leftovers can still be found and removed, with its entry in the list.")
		} else {
			ui.Text(c, "The app's own uninstaller will open. When it's done, you can scan for what it left behind.")
			if a.CanQuiet() {
				ui.Checkbox(c.Key("quiet"), &m.quiet, "Uninstall quietly, without the uninstaller's windows")
			}
		}
		// What an app installed for everyone leaves is mostly where only
		// an administrator can remove it: say so while it can be helped.
		if a.AllUsers && !m.elevated {
			m.infoBar(c, t, infoWarning, "Not running as administrator",
				"Leftovers in Program Files and the system registry can't be removed without it.", func() {
					if hyperlink(c, "elevate", "Restart as administrator").Margin(4, 0, 0, 0).Clicked() {
						m.restartElevated(c)
					}
				})
		}
	})
	m.footer(c, func() {
		if a.Broken {
			if wide(ui.PrimaryButton(c.Key("go"), "Find leftovers")).AutoFocus().Clicked() {
				m.scan()
			}
		} else if wide(ui.PrimaryButton(c.Key("go"), "Uninstall")).AutoFocus().Clicked() {
			m.uninstall()
		}
		if wide(ui.Button(c.Key("cancel"), "Cancel")).Clicked() {
			m.close()
		}
	})
}

func (m *model) runningView(c *ui.Context, t *ui.Theme) {
	a := m.target
	c.After(400 * time.Millisecond) // to see the uninstaller exit
	msg := "Follow the steps in the uninstaller's window."
	if m.quiet {
		msg = "Removing it quietly. This can take a minute."
	}
	exited := m.run != nil && m.run.MainExited()
	if exited {
		msg = "Waiting for the uninstaller to finish its work…"
	}
	m.body(c, "Uninstalling", func() {
		m.appLine(c, t, a)
		ui.Text(c, msg)
		ui.Progress(c, -1).Label("Uninstalling")
	})
	m.footer(c, func() {
		if wide(ui.Button(c.Key("continue"), "It's done — continue")).Disabled(!exited).
			Tooltip("When the uninstaller's window has closed").Clicked() {
			m.run, m.gone, m.phase = nil, !a.StillInstalled(), phaseFinished
		}
	})
}

func (m *model) finishedView(c *ui.Context, t *ui.Theme) {
	a := m.target
	var kind infoKind
	var title, head, msg, scanLabel, closeLabel string
	switch {
	case m.runErr != nil:
		kind, title, head = infoError, "Couldn't uninstall", "The uninstaller couldn't start"
		msg = m.runErr.Error() + ". You can still look for what the app left behind."
		scanLabel, closeLabel = "Find leftovers", "Close"
	case m.gone:
		// It went as it should: what is left to decide is the scan.
		title = "Scan for leftovers?"
		scanLabel, closeLabel = "Scan for leftovers", "Not now"
	default:
		kind, title, head = infoWarning, "Still installed", a.Name+" still looks installed"
		msg = "The uninstaller may have been canceled, or may need a restart to finish. Nothing is chosen for removal while the app is installed."
		scanLabel, closeLabel = "Scan anyway", "Close"
	}
	m.body(c, title, func() {
		if m.gone && m.runErr == nil {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				glyph(c, glyphCompleted).TextColor(t.Success)
				ui.Text(c, a.Name+" was uninstalled.").FontWeight(600).Shrink(1)
			})
			ui.Text(c, "Uninstallers often leave folders, settings and registry entries behind. "+
				"The scan finds them and lets you choose what to remove.")
			return
		}
		m.infoBar(c, t, kind, head, "")
		ui.Text(c, msg)
	})
	m.footer(c, func() {
		if wide(ui.PrimaryButton(c.Key("scan"), scanLabel)).AutoFocus().Clicked() {
			m.scan()
		}
		if wide(ui.Button(c.Key("close"), closeLabel)).Clicked() {
			m.close()
		}
	})
}

func (m *model) busy(c *ui.Context, t *ui.Theme, title, msg string) {
	m.body(c, title, func() {
		m.appLine(c, t, m.target)
		ui.Text(c, msg)
		ui.Progress(c, -1).Label(title)
	})
}

// leftoverRows lays the leftovers out in the table: files and folders,
// then the registry, each under its header.
func (m *model) leftoverRows() {
	if len(m.rowsItems) == len(m.leftovers) && (len(m.leftovers) == 0 || &m.rowsItems[0] == &m.leftovers[0]) {
		return
	}
	m.rowsItems = m.leftovers
	m.fileCount, m.registryCount = 0, 0
	m.rows = m.rows[:0]
	for _, section := range []int{rowFiles, rowRegistry} {
		at := len(m.rows)
		for i, l := range m.leftovers {
			if l.IsRegistry() == (section == rowRegistry) {
				if section == rowFiles {
					m.fileCount++
				} else {
					m.registryCount++
				}
				if len(m.rows) == at {
					m.rows = append(m.rows, section)
				}
				m.rows = append(m.rows, i)
			}
		}
	}
}

func (m *model) resultsView(c *ui.Context, t *ui.Theme) {
	items := m.leftovers
	var chosen int
	var size, chosenSize int64
	for _, l := range items {
		size += l.Size
		if l.Selected {
			chosen++
			chosenSize += l.Size
		}
	}
	if len(items) == 0 {
		m.body(c, "No leftovers", func() {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				glyph(c, glyphCompleted).TextColor(t.Success)
				ui.Text(c, m.target.Name+" left nothing behind.").Shrink(1)
			})
		})
		m.footer(c, func() {
			if wide(ui.PrimaryButton(c.Key("done"), "Done")).AutoFocus().Clicked() {
				m.close()
			}
		})
		return
	}
	m.leftoverRows()
	// Keep a stable title area as the summary changes during measurement.
	ui.Row(c).Height(resultsHeader).Padding(8, contentInset).Gap(16).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Row(c).Grow(1).Shrink(1).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Leftovers of "+m.target.Name).Font("Segoe UI Variable Display, Segoe UI").
				FontSize(16).FixedLineHeight(20).FontWeight(600).SingleLine().Shrink(1).Tooltip("Leftovers of " + m.target.Name)
			summary := plural(len(items), "item")
			if size > 0 {
				summary += " · " + formatBytes(size)
			}
			if m.sizing {
				summary += " · Measuring sizes…"
			}
			ui.Text(c, summary).FontSize(12).FixedLineHeight(16).TextColor(t.TextMuted).SingleLine().Shrink(0)
		})
		ui.Toolbar(c, func() {
			if ui.Button(c.Key("all"), "Select all").Height(controlHeight).Padding(6, 10).Clicked() {
				for i := range m.leftovers {
					m.leftovers[i].Selected = true
				}
			}
			if ui.Button(c.Key("none"), "Select none").Height(controlHeight).Padding(6, 10).Clicked() {
				for i := range m.leftovers {
					m.leftovers[i].Selected = false
				}
			}
		}).Label("Selection").Shrink(0)
	})
	m.rowList.Key = func(i int) any {
		if r := m.rows[i]; r >= 0 {
			return m.leftovers[r].Display()
		}
		return m.rows[i]
	}
	m.rowList.Header = func(i int) bool { return m.rows[i] < 0 }
	m.rowList.Label = func(i int) string {
		if r := m.rows[i]; r >= 0 {
			return m.leftovers[r].Display()
		}
		return ""
	}
	// The table shows its rows whole, as far as the window has room for
	// them with the title and the buttons.
	w, h := c.Size()
	stacked := w < 840
	footerHeight := float32(48)
	if stacked {
		footerHeight = 72
	}
	room := max(80, h*0.9-resultsHeader-footerHeight-2)
	ui.Table(c.Key("leftovers"), &m.rowList, leftoverColumns, len(m.rows), func(row, col int) { m.leftoverCell(c, t, row, col) }).
		Height(m.leftoversHeight(room)).Shrink(1).Margin(0, tableInset).BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke).Label("Leftovers")
	note := func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Grow(1).Shrink(1).Children(func() {
			glyph(c, glyphInfo).FontSize(14).TextColor(t.TextMuted)
			ui.Text(c, "Files go to the Recycle Bin. Registry entries are backed up first.").
				FontSize(12).FixedLineHeight(16).TextColor(t.TextMuted).Shrink(1)
		})
	}
	ui.Column(c).Padding(8, contentInset).Gap(8).Shrink(0).
		Background(m.pal.footer).BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke).Children(func() {
		if stacked {
			note()
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Justify(ui.End).Children(func() {
			if !stacked {
				note()
			}
			label := "Remove " + plural(chosen, "item")
			if chosenSize > 0 {
				label += " (" + formatBytes(chosenSize) + ")"
			}
			if ui.PrimaryButton(c.Key("remove"), label).MinWidth(180).Height(controlHeight).Disabled(chosen == 0 || m.sizing).Clicked() {
				var picked []Leftover
				for _, l := range m.leftovers {
					if l.Selected {
						picked = append(picked, l)
					}
				}
				m.clean(picked, false)
			}
			if ui.Button(c.Key("skip"), "Skip").MinWidth(96).Height(controlHeight).Clicked() {
				m.close()
			}
		})
	})
}

// leftoversHeight fits whole rows on first opening; scrolling still reaches
// every item. Do not leave a section header without its first item.
func (m *model) leftoversHeight(room float32) float32 {
	h := float32(tableHeader + 2)
	for _, r := range m.rows {
		next := float32(leftoverRow)
		if r < 0 {
			next = headerRow
			if h+next+leftoverRow > room {
				break
			}
		}
		if h+next > room {
			break
		}
		h += next
	}
	return h
}

func (m *model) leftoverCell(c *ui.Context, t *ui.Theme, row, col int) {
	r := m.rows[row]
	if r < 0 { // a section's header
		title, g := "Files and folders", glyphFolder
		if r == rowRegistry {
			title, g = "Registry", glyphKey
		}
		n := m.fileCount
		if r == rowRegistry {
			n = m.registryCount
		}
		ui.Row(c).MinHeight(headerRow - 12).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Size(20, 20).Shrink(0).Center().Children(func() { glyph(c, g).FontSize(14) })
			ui.Text(c, title).FontSize(12.5).FixedLineHeight(18)
			ui.Text(c, fmt.Sprint(n)).FontSize(12).FixedLineHeight(16).FontWeight(400).TextColor(t.TextMuted)
		})
		return
	}
	l := &m.leftovers[r]
	switch col {
	case 0:
		ui.Checkbox(c.Key("check"), &l.Selected, "").Label(l.Display())
	case 1:
		g := glyphFolder
		switch {
		case l.Kind == KindFile && strings.EqualFold(filepath.Ext(l.Path), ".lnk"):
			g = glyphLink
		case l.Kind == KindFile:
			g = glyphFile
		case l.IsRegistry():
			g = glyphKey
		}
		name, parent := shortPath(l)
		why, doubt := splitReason(l.Reason)
		ui.Row(c).MinHeight(leftoverRow - 12).Gap(12).AlignItems(ui.Center).Shrink(1).Grow(1).Tooltip(l.Display() + "\n" + l.Reason).Children(func() {
			ui.Box(c).Size(20, 20).Shrink(0).Center().Children(func() { glyph(c, g).FontSize(18).TextColor(t.TextMuted) })
			ui.Column(c).Gap(4).Grow(1).Shrink(1).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, name).FixedLineHeight(18).SingleLine().Shrink(1)
					switch {
					case doubt != "":
						m.tag(c, t, doubt, m.pal.warning).Label("Possible match: " + doubt)
					case !l.Sure:
						m.tag(c, t, "Possible match", m.pal.warning)
					}
				})
				ui.Text(c, why+" · "+parent).FontSize(12).FixedLineHeight(14).TextColor(t.TextMuted).SingleLine().Tooltip(l.Display() + "\n" + l.Reason)
			})
		})
	case 2:
		ui.Text(c, formatBytes(l.Size)).FontSize(13).FontFeatures("tnum").SingleLine()
	}
}

func (m *model) reportView(c *ui.Context, t *ui.Theme) {
	r := m.result
	title := "Leftovers removed"
	if len(r.Failed) > 0 {
		title = "Some leftovers weren't removed"
	}
	var files []Leftover
	m.body(c, title, func() {
		// What came off, and the room it freed, is the result: it leads.
		if r.Removed > 0 {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				glyph(c, glyphCompleted).TextColor(t.Success)
				msg := "Removed " + plural(r.Removed, "item")
				if r.Freed > 0 {
					msg += " · " + formatBytes(r.Freed) + " freed"
				}
				ui.Text(c, msg).FontWeight(600)
			})
		}
		if len(r.Failed) > 0 {
			m.infoBar(c, t, infoWarning, plural(len(r.Failed), "item")+" couldn't be removed",
				"Files in use may come off after closing the program that holds them, or after a restart.")
		}
		defer func() {
			if r.BackupDir != "" {
				ui.Row(c).Gap(6).AlignItems(ui.Center).Wrap().Children(func() {
					ui.Text(c, "Registry entries were backed up as .reg files:").FontSize(12).TextColor(t.TextMuted)
					if hyperlink(c, "backups", "Open backup folder").Clicked() {
						mygo.Shell.OpenPath(r.BackupDir)
					}
				})
			}
		}()
		if len(r.Failed) == 0 {
			return
		}
		ui.Scroll(c.Key("failed")).MaxHeight(260).Shrink(1).Border(1, m.pal.stroke).Radius(4).Children(func() {
			for i, f := range r.Failed {
				if !f.Item.IsRegistry() {
					files = append(files, f.Item)
				}
				row := ui.Row(c).Padding(12, 16).Gap(12).AlignItems(ui.Start)
				if i > 0 {
					row.BorderWidth(1, 0, 0, 0).BorderColor(m.pal.stroke)
				}
				row.Children(func() {
					ui.Box(c).Size(16, 20).Shrink(0).Center().Children(func() { glyph(c, glyphError).FontSize(14).TextColor(t.Danger) })
					ui.Column(c).Grow(1).Shrink(1).Gap(4).Children(func() {
						ui.Text(c, f.Item.Display()).FontSize(12.5)
						ui.Text(c, f.Err.Error()).FontSize(12).TextColor(t.TextMuted)
					})
				})
			}
		})
	})
	m.footer(c, func() {
		narrow := len(r.Failed) == 0
		done := ui.PrimaryButton(c.Key("done"), "Done").MinHeight(controlHeight).AutoFocus()
		if narrow {
			wide(done)
		} else {
			done.MinWidth(120)
		}
		if done.Clicked() {
			m.close()
		}
		if len(files) > 0 && ui.Button(c.Key("permanent"), "Delete them permanently").
			MinHeight(controlHeight).Tooltip("Skips the Recycle Bin: they can't be put back").Clicked() {
			m.clean(files, true)
		}
	})
}

type infoKind int

const (
	infoInfo infoKind = iota
	infoSuccess
	infoWarning
	infoError
)

// infoBar is Windows 11's InfoBar: a message of a kind, with its icon on a
// fill of its color.
func (m *model) infoBar(c *ui.Context, t *ui.Theme, kind infoKind, title, msg string, action ...func()) {
	bg, color, g := m.pal.info, t.Accent, glyphInfo
	switch kind {
	case infoSuccess:
		bg, color, g = m.pal.success, t.Success, glyphCompleted
	case infoWarning:
		bg, color, g = m.pal.warning, t.Warning, glyphWarning
	case infoError:
		bg, color, g = m.pal.errBg, t.Danger, glyphError
	}
	ui.Row(c).Padding(12, 16).Gap(12).Radius(4).Background(bg).Border(1, m.pal.stroke).AlignItems(ui.Start).Children(func() {
		ui.Box(c).Size(16, 20).Shrink(0).Center().Children(func() { glyph(c, g).TextColor(color) })
		ui.Column(c).Grow(1).Shrink(1).Gap(4).Children(func() {
			if title != "" {
				ui.Text(c, title).FontWeight(600)
			}
			if msg != "" {
				ui.Text(c, msg).FontSize(12.5)
			}
			for _, fn := range action {
				fn()
			}
		})
	})
}

// byLine tells, under an app's name in a dialog, which app it is.
func byLine(a *App) string {
	var parts []string
	if a.Publisher != "" {
		parts = append(parts, a.Publisher)
	}
	if a.SizeKB > 0 {
		parts = append(parts, formatBytes(int64(a.SizeKB)*1024))
	}
	return strings.Join(parts, " · ")
}

// shortPath names a leftover by its last part, which tells what it is,
// with where it is beside it: the hives as regedit's short names.
func shortPath(l *Leftover) (name, parent string) {
	if !l.IsRegistry() {
		return filepath.Base(l.Path), filepath.Dir(l.Path)
	}
	hive := shortHiveName(l.Hive)
	if l.Kind == KindValue {
		return l.Value, hive + `\` + l.Path
	}
	full := hive + `\` + l.Path
	i := strings.LastIndexByte(full, '\\')
	return full[i+1:], full[:i]
}

// splitReason parts a leftover's reason from its doubt, as "Folder named
// after the app — may hold your own files".
func splitReason(r string) (why, doubt string) {
	why, doubt, _ = strings.Cut(r, " — ")
	if doubt != "" {
		doubt = strings.ToUpper(doubt[:1]) + doubt[1:]
	}
	return why, doubt
}

func formatBytes(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("Jan 2, 2006")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
