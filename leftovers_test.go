package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"Mozilla Firefox (x64 en-US)":         "Mozilla Firefox",
		"7-Zip 23.01 (x64)":                   "7-Zip",
		"Python 3.12.1 (64-bit)":              "Python",
		"Notepad++ (64-bit x64)":              "Notepad++",
		"VLC media player":                    "VLC media player",
		"Microsoft Visual Studio Code (User)": "Microsoft Visual Studio Code",
		"Blender 4.2":                         "Blender",
		"Git version 2.45.1":                  "Git",
		"Microsoft 365 - en-us":               "Microsoft 365",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := norm("Notepad ++"); got != "notepad++" {
		t.Errorf("norm = %q", got)
	}
	if got := cleanPublisher("Mozilla Corporation"); got != "Mozilla" {
		t.Errorf("cleanPublisher = %q", got)
	}
}

func TestCommandLine(t *testing.T) {
	sys := os.Getenv("SystemRoot") + `\System32\`
	if got := commandExe(`"C:\Program Files\App\unins000.exe" /SILENT`); got != `C:\Program Files\App\unins000.exe` {
		t.Errorf("quoted: %q", got)
	}
	if got := commandArgs(`"C:\Program Files\App\unins000.exe" /SILENT`, `C:\Program Files\App\unins000.exe`); got != "/SILENT" {
		t.Errorf("args: %q", got)
	}
	if got := commandExe(`MsiExec.exe /X{1234}`); !strings.EqualFold(got, sys+"MsiExec.exe") {
		t.Errorf("msiexec: %q", got)
	}
	if got := iconPath(`"C:\App\app.exe",0`); got != `C:\App\app.exe` {
		t.Errorf("iconPath: %q", got)
	}
	if got := iconPath(`C:\App\app.exe,-101`); got != `C:\App\app.exe` {
		t.Errorf("iconPath: %q", got)
	}
}

// fakeSystem lays out folders and registry keys as installers leave them,
// in a temporary folder and under HKCU\Software\UninstallerTest.
type fakeSystem struct {
	t                                  *testing.T
	root                               string
	programs, appData, docs, startMenu string
	regRoot                            string
}

const testKey = `Software\UninstallerTest`

func newFakeSystem(t *testing.T) *fakeSystem {
	root := t.TempDir()
	f := &fakeSystem{
		t: t, root: root,
		programs:  filepath.Join(root, "Program Files"),
		appData:   filepath.Join(root, "AppData"),
		docs:      filepath.Join(root, "Documents"),
		startMenu: filepath.Join(root, "Start Menu"),
		regRoot:   testKey,
	}
	registry.DeleteKey(registry.CURRENT_USER, testKey) // a failed run's
	deleteKeyTree(registry.CURRENT_USER, testKey)
	t.Cleanup(func() { deleteKeyTree(registry.CURRENT_USER, testKey) })
	return f
}

func (f *fakeSystem) file(path string, data []byte) string {
	f.t.Helper()
	full := filepath.Join(f.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return full
}

func (f *fakeSystem) key(path string, values map[string]string) {
	f.t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, f.regRoot+`\`+path, registry.ALL_ACCESS)
	if err != nil {
		f.t.Fatal(err)
	}
	defer k.Close()
	for name, v := range values {
		if err := k.SetStringValue(name, v); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakeSystem) scanner(app *App, others ...*App) *Scanner {
	hkcu := registry.CURRENT_USER
	s := &Scanner{
		App:          app,
		Others:       others,
		Dirs:         []string{f.programs, f.appData},
		MaybeDirs:    []string{f.docs},
		ShortcutDirs: []string{f.startMenu},
		NameKeys:     []regLoc{{hkcu, f.regRoot + `\Software`}},
		RunKeys:      []regLoc{{hkcu, f.regRoot + `\Run`}},
		Protected:    map[string]bool{},
	}
	for _, p := range []string{f.root, f.programs, f.appData, f.docs, f.startMenu} {
		s.Protected[lowerPath(p)] = true
	}
	return s
}

func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func fakeApp(name, publisher, dir string) *App {
	a := &App{
		Name:            name,
		Publisher:       publisher,
		InstallLocation: dir,
		DisplayIcon:     filepath.Join(dir, norm(name)+".exe"),
		UninstallString: `"` + filepath.Join(dir, "unins000.exe") + `"`,
		Hive:            registry.CURRENT_USER,
		KeyPath:         testKey + `\Uninstall\` + name,
	}
	a.prepare()
	return a
}

func paths(items []Leftover) map[string]Leftover {
	m := make(map[string]Leftover)
	for _, l := range items {
		m[l.Display()] = l
	}
	return m
}

func TestScanAndClean(t *testing.T) {
	f := newFakeSystem(t)
	dir := filepath.Join(f.programs, "Fake Vendor", "FakeApp")
	f.file(`Program Files\Fake Vendor\FakeApp\data\cache.bin`, make([]byte, 4096))
	f.file(`AppData\FakeApp\settings.json`, []byte(`{}`))
	f.file(`AppData\FakeApp Crashpad\dump.dmp`, []byte("x"))
	f.file(`AppData\Unrelated\keep.txt`, []byte("keep"))
	f.file(`Documents\FakeApp\my work.txt`, []byte("mine"))
	f.file(`Start Menu\FakeApp.lnk`, []byte("shortcut"))
	f.file(`Start Menu\Tools\Launch it.lnk`, append([]byte("header"), utf16le(dir+`\fakeapp.exe`)...))
	f.file(`Start Menu\Tools\Other.lnk`, []byte("other"))
	f.key(`Software\Fake Vendor\FakeApp`, map[string]string{"Theme": "dark"})
	f.key(`Software\FakeApp`, nil)
	f.key(`Software\Unrelated`, nil)
	f.key(`Run`, map[string]string{
		"FakeApp updater": `"` + dir + `\fakeapp.exe" --update`,
		"Unrelated":       `C:\Windows\notepad.exe`,
	})

	app := fakeApp("FakeApp", "Fake Vendor, Inc.", dir)
	app.Name = "FakeApp 2.1 (x64)"
	items := f.scanner(app).Scan()
	found := paths(items)

	hkcu := `HKEY_CURRENT_USER\` + testKey
	want := map[string]bool{ // path: selected
		filepath.Join(f.programs, "Fake Vendor"):             true, // the publisher's folder held only the app
		filepath.Join(f.appData, "FakeApp"):                  true,
		filepath.Join(f.appData, "FakeApp Crashpad"):         false, // a possible match
		filepath.Join(f.docs, "FakeApp"):                     false, // may be the user's
		filepath.Join(f.startMenu, "FakeApp.lnk"):            true,
		filepath.Join(f.startMenu, "Tools", "Launch it.lnk"): true, // leads into the install folder
		hkcu + `\Software\Fake Vendor`:                       true,
		hkcu + `\Software\FakeApp`:                           true,
		hkcu + `\Run  ›  FakeApp updater`:                    true,
	}
	for p, sel := range want {
		l, ok := found[p]
		if !ok {
			t.Errorf("not found: %s", p)
			continue
		}
		if l.Selected != sel {
			t.Errorf("%s: selected %v, want %v (%s)", p, l.Selected, sel, l.Reason)
		}
	}
	for p := range found {
		if !want[p] {
			if _, ok := want[p]; !ok {
				t.Errorf("unexpected leftover: %s (%s)", p, found[p].Reason)
			}
		}
	}

	// Remove what is selected, files for good rather than to the Recycle Bin.
	Trash = os.RemoveAll
	var picked []Leftover
	for _, l := range items {
		if l.Selected {
			picked = append(picked, l)
		}
	}
	backups := filepath.Join(f.root, "backups")
	res := Clean(picked, app.Name, backups, false)
	if len(res.Failed) > 0 {
		t.Fatalf("failed: %v", res.Failed)
	}
	if res.Removed != len(picked) || res.Freed < 4096 {
		t.Errorf("removed %d of %d, freed %d", res.Removed, len(picked), res.Freed)
	}
	for _, p := range []string{filepath.Join(f.programs, "Fake Vendor"), filepath.Join(f.appData, "FakeApp")} {
		if exists(p) {
			t.Errorf("still there: %s", p)
		}
	}
	for _, p := range []string{filepath.Join(f.appData, "Unrelated", "keep.txt"), filepath.Join(f.docs, "FakeApp", "my work.txt"), filepath.Join(f.startMenu, "Tools", "Other.lnk")} {
		if !exists(p) {
			t.Errorf("removed what it should keep: %s", p)
		}
	}
	if keyExists(registry.CURRENT_USER, testKey+`\Software\Fake Vendor`) || keyExists(registry.CURRENT_USER, testKey+`\Software\FakeApp`) {
		t.Error("keys still there")
	}
	if !keyExists(registry.CURRENT_USER, testKey+`\Software\Unrelated`) {
		t.Error("removed an unrelated key")
	}
	if hasValue(registry.CURRENT_USER, testKey+`\Run`, "FakeApp updater") || !hasValue(registry.CURRENT_USER, testKey+`\Run`, "Unrelated") {
		t.Error("Run values wrong")
	}
	backed, _ := filepath.Glob(filepath.Join(res.BackupDir, "*.reg"))
	if len(backed) != 3 { // two keys and the values
		t.Errorf("backups: %v", backed)
	}
	if data, err := os.ReadFile(filepath.Join(res.BackupDir, "values.reg")); err != nil || !strings.Contains(decodeUTF16(data), `"FakeApp updater"=hex(1):`) {
		t.Errorf("values.reg: %v %q", err, decodeUTF16(data))
	}
}

// Another app of the same publisher keeps the publisher's folder and key,
// and what it may share with the app is not chosen.
func TestScanKeepsOtherApps(t *testing.T) {
	f := newFakeSystem(t)
	dir := filepath.Join(f.programs, "Fake Vendor", "FakeApp")
	other := filepath.Join(f.programs, "Fake Vendor", "Fake Tools")
	f.file(`Program Files\Fake Vendor\FakeApp\a.txt`, []byte("a"))
	f.file(`Program Files\Fake Vendor\Fake Tools\b.txt`, []byte("b"))
	f.key(`Software\Fake Vendor\FakeApp`, nil)
	f.key(`Software\Fake Vendor\Fake Tools`, nil)

	app := fakeApp("FakeApp", "Fake Vendor", dir)
	tools := fakeApp("Fake Tools", "Fake Vendor", other)
	found := paths(f.scanner(app, tools).Scan())

	hkcu := `HKEY_CURRENT_USER\` + testKey
	for _, p := range []string{filepath.Join(f.programs, "Fake Vendor"), hkcu + `\Software\Fake Vendor`, other, hkcu + `\Software\Fake Vendor\Fake Tools`} {
		if _, ok := found[p]; ok {
			t.Errorf("would remove %s", p)
		}
	}
	for _, p := range []string{dir, hkcu + `\Software\Fake Vendor\FakeApp`} {
		if l, ok := found[p]; !ok || !l.Selected {
			t.Errorf("missing %s", p)
		}
	}
}

func TestRemovableProtectsFolders(t *testing.T) {
	s := &Scanner{Protected: map[string]bool{lowerPath(`C:\Program Files`): true, lowerPath(`C:\Users\Ada\AppData\Roaming`): true}}
	for p, want := range map[string]bool{
		`C:\`:                               false,
		`C:\Program Files`:                  false,
		`C:\Users`:                          false, // holds a protected folder
		`C:\Program Files\Microsoft`:        false,
		`C:\Program Files\Common Files`:     false,
		`C:\Program Files\FakeApp`:          true,
		`C:\Users\Ada\AppData\Roaming\Fake`: true,
	} {
		if got := s.removable(p); got != want {
			t.Errorf("removable(%s) = %v", p, got)
		}
	}
}

func decodeUTF16(b []byte) string {
	if len(b) >= 2 {
		b = b[2:]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestLoadApps(t *testing.T) {
	apps := LoadApps()
	if len(apps) == 0 {
		t.Skip("no apps installed")
	}
	if !slices.IsSortedFunc(apps, func(a, b *App) int { return strings.Compare(a.lowerName, b.lowerName) }) {
		t.Error("not sorted")
	}
	for _, a := range apps {
		if a.Name == "" || a.Command(false) == "" {
			t.Errorf("incomplete: %+v", a)
		}
	}
	t.Logf("%d apps", len(apps))
}

// An app from elsewhere, as OpenCode, keeps its files in the XDG folders of
// the user's home and in Temp, registers a link handler and a notification
// server, may have come with a command from npm, and Windows remembers it by
// its app ID and by programs of its older installs.
func TestScanFindsToolLeftovers(t *testing.T) {
	f := newFakeSystem(t)
	dir := filepath.Join(f.programs, "ai.opencode.desktop")
	f.file(`Program Files\ai.opencode.desktop\OpenCode.exe`, []byte("exe"))
	f.file(`.config\opencode\opencode.json`, []byte(`{}`))
	f.file(`.local\share\opencode\opencode.db`, []byte("db"))
	f.file(`Temp\opencode\x.tmp`, []byte("x"))
	f.file(`AppData\@opencodedesktop-updater\installer.exe`, []byte("x"))
	f.file(`npm\opencode.cmd`, []byte("@node opencode"))
	f.file(`npm\opencode.ps1`, []byte("node opencode"))
	f.file(`npm\npm.cmd`, []byte("@node npm"))
	f.file(`npm\node_modules\opencode-ai\package.json`, []byte(`{}`))
	f.file(`npm\node_modules\typescript\package.json`, []byte(`{}`))
	f.key(`Classes\opencode`, map[string]string{"": "URL:opencode", "URL Protocol": ""})
	f.key(`Classes\opencode\shell\open\command`, map[string]string{"": `"` + dir + `\OpenCode.exe" "%1"`})
	f.key(`Classes\OpenCodeFile`, map[string]string{"": "not a handler"})
	gone := filepath.Join(f.root, "Old", "@opencodedesktop", "OpenCode.exe") // an older install's program
	notepad := filepath.Join(os.Getenv("SystemRoot"), "notepad.exe")
	f.key(`Usage`, map[string]string{
		dir + `\OpenCode.exe.FriendlyAppName`: "OpenCode", gone + ".ApplicationCompany": "OpenCode",
		"ai.opencode.desktop": "", notepad + ".FriendlyAppName": "Notepad", "Microsoft.Windows.Explorer": "",
	})
	f.key(`Notifications\ai.opencode.desktop`, nil)
	f.key(`Notifications\W~ai.opencode.desktop`, nil)
	f.key(`Notifications\Microsoft.Windows.Explorer`, nil)
	f.key(`CLSID\{6CD27474-6CF3-4B67-956A-804CA14AFDF3}\LocalServer32`, map[string]string{"": gone})
	f.key(`CLSID\{00000000-0000-0000-0000-000000000001}\LocalServer32`, map[string]string{"": notepad})
	f.key(`Software\d074f30d-5f88-5885-b075-be1348cc7676`, map[string]string{"InstallLocation": dir})
	f.file(`Downloads\opencode-desktop-win-x64.exe`, []byte("x"))
	f.file(`Downloads\other-setup.exe`, []byte("x"))

	app := fakeApp("OpenCode 2.2.5", "OpenCode", dir)
	app.DisplayIcon = filepath.Join(dir, "OpenCode.exe") // the test folder is in Temp, so InstallLocation stays
	app.KeyPath = testKey + `\Uninstall\d074f30d-5f88-5885-b075-be1348cc7676`
	app.prepare()
	s := f.scanner(app)
	s.Dirs = append(s.Dirs, filepath.Join(f.root, ".config"), filepath.Join(f.root, ".local", "share"), filepath.Join(f.root, "Temp"))
	s.NpmDirs = []string{filepath.Join(f.root, "npm")}
	hkcuKey := registry.CURRENT_USER
	s.ClassKeys = []regLoc{{hkcuKey, f.regRoot + `\Classes`}}
	s.UsageKeys = []regLoc{{hkcuKey, f.regRoot + `\Usage`}}
	s.IDKeys = []regLoc{{hkcuKey, f.regRoot + `\Notifications`}}
	s.ServerKeys = []regLoc{{hkcuKey, f.regRoot + `\CLSID`}}
	s.DownloadDirs = []string{filepath.Join(f.root, "Downloads")}
	found := paths(s.Scan())

	hkcu := `HKEY_CURRENT_USER\` + testKey
	want := map[string]bool{ // path: selected
		dir: true,
		filepath.Join(f.root, ".config", "opencode"):                       true,
		filepath.Join(f.root, ".local", "share", "opencode"):               true,
		filepath.Join(f.root, "Temp", "opencode"):                          true,
		filepath.Join(f.appData, "@opencodedesktop-updater"):               false,
		filepath.Join(f.root, "npm", "opencode.cmd"):                       false, // the command line tool is another install
		filepath.Join(f.root, "npm", "opencode.ps1"):                       false,
		filepath.Join(f.root, "npm", "node_modules", "opencode-ai"):        false,
		hkcu + `\Classes\opencode`:                                         true,
		filepath.Join(f.root, "Downloads", "opencode-desktop-win-x64.exe"): false, // the user's own file
		hkcu + `\Usage  ›  ` + dir + `\OpenCode.exe.FriendlyAppName`:       true,
		hkcu + `\Usage  ›  ` + gone + `.ApplicationCompany`:                true,
		hkcu + `\Usage  ›  ai.opencode.desktop`:                            true,
		hkcu + `\Notifications\ai.opencode.desktop`:                        true,
		hkcu + `\Notifications\W~ai.opencode.desktop`:                      true,
		hkcu + `\CLSID\{6CD27474-6CF3-4B67-956A-804CA14AFDF3}`:             true,
		hkcu + `\Software\d074f30d-5f88-5885-b075-be1348cc7676`:            true,
	}
	for p, sel := range want {
		l, ok := found[p]
		if !ok {
			t.Errorf("not found: %s", p)
			continue
		}
		if l.Selected != sel {
			t.Errorf("%s: selected %v, want %v (%s)", p, l.Selected, sel, l.Reason)
		}
	}
	for p := range found {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected leftover: %s (%s)", p, found[p].Reason)
		}
	}
}
