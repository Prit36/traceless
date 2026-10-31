package main

import (
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows/registry"
)

func sampleModel() *model {
	m := newModel(false)
	day := func(y, mo, d int) time.Time { return time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.Local) }
	m.apps = []*App{
		{Name: "7-Zip 24.08 (x64)", Version: "24.08", Publisher: "Igor Pavlov", SizeKB: 5800, InstallDate: day(2025, 3, 2), AllUsers: true, UninstallString: `C:\Program Files\7-Zip\Uninstall.exe`, InstallLocation: `C:\Program Files\7-Zip`},
		{Name: "Discord", Version: "1.0.9175", Publisher: "Discord Inc.", SizeKB: 420000, InstallDate: day(2026, 9, 14), UninstallString: `"C:\Users\Ada\AppData\Local\Discord\Update.exe" --uninstall`},
		{Name: "Old Game Demo", Version: "0.9", Publisher: "Indie Studio", SizeKB: 2100000, InstallDate: day(2021, 1, 5), AllUsers: true, UninstallString: `C:\Games\Demo\unins000.exe`, Broken: true},
		{Name: "VLC media player", Version: "3.0.21", Publisher: "VideoLAN", SizeKB: 168000, InstallDate: day(2026, 6, 1), AllUsers: true, UninstallString: `"C:\Program Files\VideoLAN\VLC\uninstall.exe"`, QuietUninstall: `"C:\Program Files\VideoLAN\VLC\uninstall.exe" /S`, InstallLocation: `C:\Program Files\VideoLAN\VLC`},
	}
	for i, a := range m.apps {
		a.Hive, a.KeyPath = registry.CURRENT_USER, testKey+`\Uninstall\`+string(rune('a'+i))
		a.lowerName, a.lowerPublisher = strings.ToLower(a.Name), strings.ToLower(a.Publisher)
		a.sizeText, a.dateText = formatBytes(int64(a.SizeKB)*1024), formatDate(a.InstallDate)
	}
	m.loading = false
	m.filter()
	return m
}

func snapshot(t *testing.T, tt *ui.Tester, name string) {
	dir := os.Getenv("SNAPSHOTS")
	if dir == "" {
		return
	}
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, tt.Image())
}

func TestViewFlow(t *testing.T) {
	m := sampleModel()
	tt := ui.NewTester(m.view, 1060, 700)
	if !tt.HasText("All apps") || !tt.HasText("VLC media player") || !tt.HasText("Uninstaller missing") || !tt.HasText("4 apps · 2.57 GB") {
		t.Fatalf("texts: %q", tt.Texts())
	}
	// The table leaves the version to the details pane.
	if tt.HasText("Version") {
		t.Fatal("the table has a Version column")
	}
	snapshot(t, tt, "1-list")

	// Search narrows the list.
	m.search.Focus()
	tt.Frame()
	tt.Type("vlc")
	if len(m.shown) != 1 || m.shown[0].Name != "VLC media player" {
		t.Fatalf("search shows %d apps", len(m.shown))
	}
	snapshot(t, tt, "2-search")

	// Uninstall asks first, offering a quiet uninstall where there is one.
	if err := tt.Click("Uninstall"); err != nil {
		t.Fatal(err)
	}
	if m.phase != phaseConfirm || !tt.HasText("Uninstall this app?") || !tt.HasText("Uninstall quietly, without the uninstaller's windows") || !tt.HasText("Not running as administrator") {
		t.Fatalf("confirm: phase %d, %q", m.phase, tt.Texts())
	}
	snapshot(t, tt, "3-confirm")
	tt.Key(0, ui.KeyEscape)
	if m.phase != phaseNone {
		t.Fatal("Escape kept the dialog")
	}

	// After the uninstaller: offer the scan.
	m.target, m.gone, m.phase = m.shown[0], true, phaseFinished
	tt.Frame()
	if !tt.HasText("Scan for leftovers?") || !tt.HasText("VLC media player was uninstalled.") {
		t.Fatalf("finished: %q", tt.Texts())
	}
	snapshot(t, tt, "4-finished")

	m.runErr, m.gone = nil, false
	tt.Frame()
	snapshot(t, tt, "4b-still-installed")

	// The leftovers, the sure ones chosen.
	m.leftovers = []Leftover{
		{Kind: KindFolder, Path: `C:\Program Files\VideoLAN`, Reason: "Publisher's folder, holding only the app's files", Sure: true, Size: 41 << 20},
		{Kind: KindFolder, Path: `C:\Users\Ada\AppData\Roaming\vlc`, Reason: "Folder named after the app", Sure: true, Size: 380 << 10},
		{Kind: KindFile, Path: `C:\Users\Public\Desktop\VLC media player.lnk`, Reason: "Shortcut named after the app", Sure: true, Size: 1200},
		{Kind: KindFolder, Path: `C:\Users\Ada\AppData\Local\Microsoft\WinGet\Packages\VideoLAN.VLC_Microsoft.Winget.Source_8wekyb3d8bbwe`, Reason: "Install folder", Sure: true, Size: 4400 << 10},
		{Kind: KindFolder, Path: `C:\Users\Ada\Documents\VLC Recordings`, Reason: "Folder named after the app — may hold your own files", Size: 900 << 20},
		{Kind: KindKey, Hive: registry.CURRENT_USER, Path: `Software\VideoLAN`, Reason: "Publisher's key, holding only the app's settings", Sure: true},
		{Kind: KindKey, Hive: registry.LOCAL_MACHINE, Path: `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\vlc.exe`, Reason: "Program path", Sure: true},
		{Kind: KindValue, Hive: registry.CURRENT_USER, Path: `Software\Microsoft\Windows\CurrentVersion\Run`, Value: "VLC tray", Reason: "Starts with Windows", Sure: true},
	}
	for i := range m.leftovers {
		m.leftovers[i].Selected = m.leftovers[i].Sure
	}
	results := slices.Clone(m.leftovers)
	m.phase = phaseResults
	tt.Frame()
	if !tt.HasText("Leftovers of VLC media player") || !tt.HasText("May hold your own files") || !tt.HasText("Run") {
		t.Fatalf("results: %q", tt.Texts())
	}
	snapshot(t, tt, "5-results")
	last, ok := tt.Find(`Starts with Windows · HKCU\Software\Microsoft\Windows\CurrentVersion\Run`)
	remove, _ := tt.Find("Remove 7 items (45.7 MB)")
	if !ok || last.Y+last.H > remove.Y-8 {
		t.Fatal("the last leftover is clipped at the default window size")
	}
	tt.SetSize(920, 660)
	tt.Frame()
	snapshot(t, tt, "5b-results-small")
	tt.SetSize(1060, 700)
	tt.Frame()
	if err := tt.Click("Select none"); err != nil {
		t.Fatal(err)
	}
	for _, l := range m.leftovers {
		if l.Selected {
			t.Fatal("Select none left one chosen")
		}
	}
	tt.Click("Select all")

	// What removing them did.
	m.result = CleanResult{Removed: 6, Freed: 942 << 20, BackupDir: `C:\Users\Ada\AppData\Local\Uninstaller\Registry backups\VLC`,
		Failed: []Failure{{m.leftovers[1], errors.New("The process cannot access the file because it is being used by another process.")}}}
	m.phase = phaseReport
	tt.Frame()
	if !tt.HasText("Delete them permanently") || !tt.HasText("Open backup folder") {
		t.Fatalf("report: %q", tt.Texts())
	}
	snapshot(t, tt, "6-report")

	// A broken entry goes straight to its leftovers.
	m.phase = phaseNone
	m.query = ""
	m.filter()
	for i, a := range m.shown {
		if a.Broken {
			m.chosen = i
		}
	}
	tt.Frame()
	if err := tt.Click("Remove leftovers"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("Find leftovers") {
		t.Fatalf("broken: %q", tt.Texts())
	}
	snapshot(t, tt, "7-broken")

	tt.SetDark(true)
	tt.Frame()
	snapshot(t, tt, "8-dark-dialog")
	m.phase = phaseNone
	tt.Frame()
	snapshot(t, tt, "9-dark")
	m.phase = phaseResults
	m.target, m.leftovers = m.apps[3], results
	tt.Frame()
	snapshot(t, tt, "10-dark-results")

	// A narrow window shows the navigation pane's icons only.
	m.phase = phaseNone
	tt.SetDark(false)
	tt.SetSize(760, 520)
	tt.Frame()
	if slices.Contains(tt.Texts(), "0") { // the count of the recently installed
		t.Fatal("the narrow pane shows labels")
	}
	snapshot(t, tt, "11-narrow")

	// The views of the pane filter the apps.
	tt.SetSize(1060, 700)
	tt.Frame()
	if err := tt.Click("Uninstaller missing"); err != nil {
		t.Fatal(err)
	}
	if len(m.shown) != 1 || !m.shown[0].Broken {
		t.Fatalf("uninstaller missing shows %d apps", len(m.shown))
	}
}
