package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Kind is what a leftover is.
type Kind uint8

const (
	KindFolder Kind = iota
	KindFile
	KindKey   // a registry key and everything under it
	KindValue // one value of a registry key
)

// Leftover is something an app left behind.
type Leftover struct {
	Kind   Kind
	Path   string       // a file's path, or a registry key's path under Hive
	Hive   registry.Key // for registry leftovers
	Value  string       // the value's name, for KindValue
	Reason string
	// Sure is set when the leftover is the app's beyond doubt, by its
	// install folder or its exact name: it is selected by default. Others
	// are only possible matches, which the user chooses.
	Sure     bool
	Size     int64 // bytes, for files and folders
	Selected bool
}

// IsRegistry reports whether the leftover is in the registry.
func (l *Leftover) IsRegistry() bool { return l.Kind == KindKey || l.Kind == KindValue }

// Display is the leftover's full path, as the user knows it.
func (l *Leftover) Display() string {
	switch l.Kind {
	case KindKey:
		return hiveName(l.Hive) + `\` + l.Path
	case KindValue:
		return hiveName(l.Hive) + `\` + l.Path + `  ›  ` + l.Value
	}
	return l.Path
}

type regLoc struct {
	hive registry.Key
	path string
}

// Scanner finds what an app left behind, in the folders and registry keys
// apps keep their files and settings in. It looks only one level into each
// (two under a publisher's folder), which is where installers put them, so a
// scan takes a few milliseconds.
type Scanner struct {
	App    *App
	Others []*App // the apps still installed, whose files are never touched

	Dirs         []string // folders whose entries may be the app's
	MaybeDirs    []string // the same, whose entries may hold the user's own files
	ShortcutDirs []string // folders whose shortcuts may lead to the app
	NameKeys     []regLoc // keys whose subkeys may be the app's
	RunKeys      []regLoc // keys of programs that start with Windows
	AppPathKeys  []regLoc
	AppKeys      []regLoc // Classes\Applications, by program name
	MuiKeys      []regLoc // where Explorer keeps the names of programs it ran
	Protected    map[string]bool

	self      string
	otherDirs []string // where the other apps are installed, lower case
	ctx       context.Context
}

// NewScanner prepares a scan of the usual places for app, among all the
// apps installed.
func NewScanner(app *App, all []*App) *Scanner {
	s := &Scanner{App: app}
	for _, o := range all {
		if o != app && o.ID() != app.ID() {
			s.Others = append(s.Others, o)
		}
	}
	env := os.Getenv
	local, roaming, profile := env("LOCALAPPDATA"), env("APPDATA"), env("USERPROFILE")
	s.Dirs = nonEmpty(
		env("ProgramFiles"), env("ProgramFiles(x86)"), env("ProgramW6432"),
		env("CommonProgramFiles"), env("CommonProgramFiles(x86)"),
		env("ProgramData"), roaming, local, filepath.Join(local, "Programs"),
		filepath.Join(profile, "AppData", "LocalLow"),
	)
	s.MaybeDirs = nonEmpty(knownFolder(windows.FOLDERID_Documents), profile)
	s.ShortcutDirs = nonEmpty(
		knownFolder(windows.FOLDERID_Programs), knownFolder(windows.FOLDERID_CommonPrograms),
		knownFolder(windows.FOLDERID_Desktop), knownFolder(windows.FOLDERID_PublicDesktop),
	)
	hkcu, hklm := registry.CURRENT_USER, registry.LOCAL_MACHINE
	s.NameKeys = []regLoc{{hkcu, `Software`}, {hklm, `SOFTWARE`}, {hklm, `SOFTWARE\WOW6432Node`}}
	s.RunKeys = []regLoc{
		{hkcu, `Software\Microsoft\Windows\CurrentVersion\Run`},
		{hkcu, `Software\Microsoft\Windows\CurrentVersion\RunOnce`},
		{hklm, `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`},
		{hklm, `SOFTWARE\Microsoft\Windows\CurrentVersion\RunOnce`},
		{hklm, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Run`},
	}
	s.AppPathKeys = []regLoc{
		{hkcu, `Software\Microsoft\Windows\CurrentVersion\App Paths`},
		{hklm, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths`},
		{hklm, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths`},
	}
	s.AppKeys = []regLoc{{hkcu, `Software\Classes\Applications`}, {hklm, `SOFTWARE\Classes\Applications`}}
	s.MuiKeys = []regLoc{{hkcu, `Software\Classes\Local Settings\Software\Microsoft\Windows\Shell\MuiCache`}}

	s.Protected = make(map[string]bool)
	for _, p := range slices.Concat(s.Dirs, s.MaybeDirs, s.ShortcutDirs, nonEmpty(
		env("SystemRoot"), filepath.Join(env("SystemRoot"), "System32"), env("SystemDrive")+`\Users`,
		env("PUBLIC"), env("TEMP"), env("OneDrive"), filepath.Join(local, "Temp"),
		filepath.Join(local, "Microsoft"), filepath.Join(roaming, "Microsoft"),
		knownFolder(windows.FOLDERID_Downloads), knownFolder(windows.FOLDERID_Pictures),
		knownFolder(windows.FOLDERID_Music), knownFolder(windows.FOLDERID_Videos),
	)) {
		s.Protected[lowerPath(p)] = true
	}
	if exe, err := os.Executable(); err == nil {
		s.self = lowerPath(filepath.Dir(exe))
	}
	return s
}

func knownFolder(id *windows.KNOWNFOLDERID) string {
	p, err := windows.KnownFolderPath(id, 0)
	if err != nil {
		return ""
	}
	return p
}

func nonEmpty(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p != "" && !slices.ContainsFunc(out, func(q string) bool { return strings.EqualFold(p, q) }) {
			out = append(out, p)
		}
	}
	return out
}

// Scan looks for the app's leftovers. Folders the scan finds still have
// their size measured, in parallel.
func (s *Scanner) Scan() []Leftover {
	return s.ScanContext(context.Background())
}

func (s *Scanner) ScanContext(ctx context.Context) []Leftover {
	items := s.Discover(ctx)
	measureContext(ctx, items, nil)
	return items
}

// Discover returns matches before the potentially expensive size walk. All
// protection checks finish before any result is offered to the user.
func (s *Scanner) Discover(ctx context.Context) []Leftover {
	if ctx.Err() != nil {
		return nil
	}
	s.ctx = ctx
	t := s.tokens()
	f := &found{seen: make(map[string]bool)}

	if dir := t.installDir; dir != "" && exists(dir) && s.removable(dir) {
		sure := depth(dir) >= 2 || t.names[norm(filepath.Base(dir))]
		f.add(Leftover{Kind: KindFolder, Path: dir, Reason: "Install folder", Sure: sure})
	}
	var jobs []func(*found)
	for _, dir := range s.Dirs {
		jobs = append(jobs, func(f *found) { s.scanDir(dir, t, f, true) })
	}
	for _, dir := range s.MaybeDirs {
		jobs = append(jobs, func(f *found) { s.scanDir(dir, t, f, false) })
	}
	for _, dir := range s.ShortcutDirs {
		jobs = append(jobs, func(f *found) { s.scanShortcuts(dir, t, f) })
	}
	jobs = append(jobs, func(f *found) { s.scanRegistry(t, f) })
	if len(jobs) <= 2 {
		for _, job := range jobs {
			if ctx.Err() == nil {
				job(f)
			}
		}
	} else {
		results := make([][]Leftover, len(jobs))
		var wg sync.WaitGroup
		for worker := range min(4, len(jobs)) {
			wg.Go(func() {
				for i := worker; i < len(jobs) && ctx.Err() == nil; i += min(4, len(jobs)) {
					local := &found{seen: make(map[string]bool)}
					jobs[i](local)
					results[i] = local.items
				}
			})
		}
		wg.Wait()
		if ctx.Err() != nil {
			return nil
		}
		// Merge in source order, keeping duplicate handling deterministic.
		for _, result := range results {
			for _, l := range result {
				f.add(l)
			}
		}
	}

	for i := range f.items {
		if ctx.Err() != nil {
			return nil
		}
		if l := &f.items[i]; l.Kind == KindFolder && l.Sure {
			if why := s.holdsOthers(l.Path, t); why != "" {
				l.Sure, l.Reason = false, l.Reason+" — "+why
			}
		}
	}
	items := f.items
	pruneNested(&items)
	keys := make([]string, len(items))
	for i := range items {
		keys[i] = strings.ToLower(items[i].Display())
	}
	sort.Stable(leftoverOrder{items, keys})
	// While the app is installed, as when its uninstaller was canceled,
	// nothing is chosen for the user.
	live := s.App.StillInstalled() && !s.App.Broken
	for i := range items {
		items[i].Selected = items[i].Sure && !live
	}
	return items
}

type leftoverOrder struct {
	items []Leftover
	keys  []string
}

func (o leftoverOrder) Len() int { return len(o.items) }
func (o leftoverOrder) Less(i, j int) bool {
	a, b := &o.items[i], &o.items[j]
	if a.IsRegistry() != b.IsRegistry() {
		return !a.IsRegistry()
	}
	if a.Sure != b.Sure {
		return a.Sure
	}
	return o.keys[i] < o.keys[j]
}
func (o leftoverOrder) Swap(i, j int) {
	o.items[i], o.items[j] = o.items[j], o.items[i]
	o.keys[i], o.keys[j] = o.keys[j], o.keys[i]
}

type found struct {
	items []Leftover
	seen  map[string]bool
}

func (f *found) add(l Leftover) {
	key := strings.ToLower(l.Display())
	if f.seen[key] {
		return
	}
	f.seen[key] = true
	f.items = append(f.items, l)
}

// tokens are the names an app's folders and keys go by.
type tokens struct {
	names      map[string]bool // exact names, normalized
	pubNames   map[string]bool // names sure within the publisher's folder or key only
	maybe      map[string]bool // names that are likely but not sure
	version    string          // the digits of the app's version, as "3147" of 3.14.7
	loose      []string        // long names that may appear within others
	publisher  string
	installDir string
	exe        string // the lower-case file name of the app's program
	shortcut   *shortcutMatcher
	shared     map[string]string // names other installed apps go by, to their names
	others     []otherApp
}

type otherApp struct{ lower, name string } // its name cleaned in lower case, and as shown

func (s *Scanner) tokens() *tokens {
	s.otherDirs = s.otherDirs[:0]
	a := s.App
	t := &tokens{
		names:      make(map[string]bool),
		pubNames:   make(map[string]bool),
		maybe:      make(map[string]bool),
		shared:     make(map[string]string),
		publisher:  norm(cleanPublisher(a.Publisher)),
		installDir: a.Dir,
		exe:        mainExe(a),
	}
	if t.installDir != "" && depth(t.installDir) >= 2 {
		t.shortcut = newShortcutMatcher(t.installDir + `\`)
	}
	for n := range appNames(a) {
		t.names[n] = true
		if len(n) >= 5 {
			t.loose = append(t.loose, n)
		}
	}
	// "Mozilla Firefox" keeps its files in Mozilla\Firefox: "Firefox" is
	// its name in its publisher's folder, only likely elsewhere.
	if p := t.publisher; p != "" {
		if n := norm(cleanName(a.Name)); strings.HasPrefix(n, p) && len(n)-len(p) >= 4 {
			if rest := n[len(p):]; !t.names[rest] && !genericNames[rest] && !protectedNames[rest] {
				t.pubNames[rest] = true
			}
		}
	}
	t.version = versionDigits(a.Name)
	if t.version == "" {
		t.version = versionDigits(a.Version)
	}
	if first := firstWord(cleanName(a.Name)); len(first) >= 4 && !t.names[first] && !genericNames[first] {
		t.maybe[first] = true
	}
	if t.exe != "" {
		if stem := norm(strings.TrimSuffix(t.exe, ".exe")); len(stem) >= 4 && !genericNames[stem] {
			t.names[stem] = true
		}
	}
	for _, o := range s.Others {
		t.others = append(t.others, otherApp{strings.ToLower(cleanName(o.Name)), o.Name})
		for n := range appNames(o) {
			t.shared[n] = o.Name
		}
		if e := mainExe(o); e != "" {
			if stem := norm(strings.TrimSuffix(e, ".exe")); len(stem) >= 4 && !genericNames[stem] {
				t.shared[stem] = o.Name
			}
		}
		if o.Dir != "" {
			s.otherDirs = append(s.otherDirs, lowerPath(o.Dir))
		}
	}
	return t
}

// appNames are the names an app surely goes by: its name without version
// and architecture, and the name of its install folder.
func appNames(a *App) map[string]bool {
	names := make(map[string]bool)
	add := func(s string) {
		if n := norm(s); len(n) >= 3 && !genericNames[n] && !protectedNames[n] {
			names[n] = true
		}
	}
	add(cleanName(a.Name))
	if d := a.Dir; d != "" && depth(d) >= 2 {
		add(cleanName(filepath.Base(d)))
	}
	return names
}

// match tells whether a folder or key named name is the app's: surely,
// possibly, and the other app it may also be. inPublisher is set for what
// is in the publisher's folder or key.
func (t *tokens) match(name string, inPublisher ...bool) (sure, maybe bool, other string) {
	n := norm(name)
	if len(n) < 3 || protectedNames[n] || genericNames[n] {
		return false, false, ""
	}
	c := n
	if cleaned := cleanName(name); cleaned != name {
		c = norm(cleaned)
	}
	switch {
	case t.names[n] || t.names[c]:
		sure = true
	case t.pubNames[n] || t.pubNames[c]:
		sure = len(inPublisher) > 0 && inPublisher[0]
		maybe = !sure
	case t.maybe[n] || t.maybe[c]:
		maybe = true
	default:
		for _, l := range t.loose {
			if strings.Contains(n, l) {
				maybe = true
				break
			}
		}
	}
	if sure || maybe {
		if o, ok := t.shared[n]; ok {
			return false, true, o
		}
		if o, ok := t.shared[c]; ok {
			return false, true, o
		}
		if sure && t.otherVersion(name) {
			return false, true, "another version"
		}
		// A folder "Python" may be Python Launcher's as well.
		if l := strings.ToLower(cleanName(name)); sure && len(l) >= 3 {
			for _, o := range t.others {
				if strings.HasPrefix(o.lower, l+" ") {
					return false, true, o.name
				}
			}
		}
	}
	return sure, maybe, ""
}

// otherVersion reports whether name says it is of another version than the
// app, as "Python 3.12" or "Python312" when the app is Python 3.14.
func (t *tokens) otherVersion(name string) bool {
	d := versionDigits(name)
	return d != "" && t.version != "" && !strings.HasPrefix(t.version, d) && !strings.HasPrefix(d, t.version)
}

var (
	dottedVersionRe = regexp.MustCompile(`\d+(\.\d+)+`)
	gluedVersionRe  = regexp.MustCompile(`(?i)[a-z](\d{2,})$`)
)

// versionDigits are the digits of the version a name holds: "314" of
// "Python 3.14" and of "Python314".
func versionDigits(s string) string {
	v := dottedVersionRe.FindString(s)
	if v == "" {
		if m := gluedVersionRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
			v = m[1]
		}
	}
	return strings.ReplaceAll(v, ".", "")
}

// holdsOthers says why a folder that looked the app's whole may not be: it
// holds an app still installed, or another version of the app.
func (s *Scanner) holdsOthers(dir string, t *tokens) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, c := norm(e.Name()), norm(cleanName(e.Name()))
		if o, ok := t.shared[n]; ok && len(n) >= 4 {
			return "also holds " + o
		}
		if o, ok := t.shared[c]; ok && len(c) >= 4 {
			return "also holds " + o
		}
		bare := strings.TrimRight(c, "0123456789")
		if (t.names[n] || t.names[c] || t.names[bare]) && t.otherVersion(e.Name()) {
			return "also holds other versions"
		}
	}
	return ""
}

// scanDir looks for the app's folders in dir, and in the folder of its
// publisher there: when the app's are all that folder holds, the whole
// folder goes.
func (s *Scanner) scanDir(dir string, t *tokens, f *found, canBeSure bool) {
	if s.cancelled() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if s.cancelled() {
			return
		}
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		name := e.Name()
		full := filepath.Join(dir, name)
		if sure, maybe, other := t.match(name); sure || maybe {
			if s.removable(full) {
				f.add(Leftover{Kind: KindFolder, Path: full, Reason: reason("Folder named after the app", other, canBeSure), Sure: sure && canBeSure})
			}
			continue
		}
		if t.publisher == "" || norm(name) != t.publisher {
			continue
		}
		children, err := os.ReadDir(full)
		if err != nil {
			continue
		}
		var mine []Leftover
		all := true
		for _, c := range children {
			sure, maybe, other := t.match(c.Name(), true)
			if !sure && !maybe {
				all = false
				continue
			}
			if !sure {
				all = false
			}
			kind := KindFolder
			if !c.IsDir() {
				kind = KindFile
			}
			mine = append(mine, Leftover{Kind: kind, Path: filepath.Join(full, c.Name()), Reason: reason("In the publisher's folder", other, canBeSure), Sure: sure && canBeSure})
		}
		if all && canBeSure && !protectedNames[t.publisher] && s.removable(full) {
			why := "Publisher's folder, holding only the app's files"
			if len(children) == 0 {
				why = "Publisher's folder, empty"
			}
			f.add(Leftover{Kind: KindFolder, Path: full, Reason: why, Sure: true})
			continue
		}
		for _, l := range mine {
			if s.removable(l.Path) {
				f.add(l)
			}
		}
	}
}

func reason(why, other string, canBeSure bool) string {
	switch {
	case other == "another version":
		return why + " — of another version"
	case other != "":
		return why + " — also matches " + other
	case !canBeSure:
		return why + " — may hold your own files"
	}
	return why
}

// scanShortcuts looks for the app's shortcuts and Start menu folders: by
// their names, or by the install folder they lead into.
func (s *Scanner) scanShortcuts(dir string, t *tokens, f *found) {
	if s.cancelled() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if s.cancelled() {
			return
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if sure, maybe, other := t.match(e.Name()); sure || maybe {
				if s.removable(full) {
					f.add(Leftover{Kind: KindFolder, Path: full, Reason: reason("Start menu folder", other, true), Sure: sure})
				}
				continue
			}
			sub, _ := os.ReadDir(full)
			for _, c := range sub {
				if !c.IsDir() {
					s.checkShortcut(filepath.Join(full, c.Name()), t, f)
				}
			}
			continue
		}
		s.checkShortcut(full, t, f)
	}
}

func (s *Scanner) checkShortcut(path string, t *tokens, f *found) {
	if s.cancelled() {
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".lnk" && ext != ".url" {
		return
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if sure, maybe, other := t.match(stem); sure || maybe {
		f.add(Leftover{Kind: KindFile, Path: path, Reason: reason("Shortcut named after the app", other, true), Sure: sure})
		return
	}
	if t.shortcut != nil && t.shortcut.mentionsFile(path) {
		f.add(Leftover{Kind: KindFile, Path: path, Reason: "Shortcut into the install folder", Sure: true})
	}
}

// fileMentions reports whether a small file, as a shortcut, holds path, in
// ANSI or UTF-16, ignoring case. Shortcuts keep their target in both.
func fileMentions(file, path string) bool {
	return newShortcutMatcher(path).mentionsFile(file)
}

type shortcutMatcher struct {
	ansi, wide []byte
	ascii      bool
}

var shortcutBuffers = sync.Pool{New: func() any { return new([64 << 10]byte) }}

func newShortcutMatcher(path string) *shortcutMatcher {
	p := strings.ToLower(path)
	m := &shortcutMatcher{ansi: []byte(p), ascii: true}
	for _, r := range p {
		if r > 127 {
			m.ascii = false
			break
		}
	}
	for _, u := range utf16.Encode([]rune(p)) {
		m.wide = append(m.wide, byte(u), byte(u>>8))
	}
	return m
}

func (m *shortcutMatcher) mentionsFile(file string) bool {
	fh, err := os.Open(file)
	if err != nil {
		return false
	}
	defer fh.Close()
	buf := shortcutBuffers.Get().(*[64 << 10]byte)
	defer shortcutBuffers.Put(buf)
	n, err := io.ReadFull(fh, buf[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false
	}
	data := buf[:n]
	// Check both UTF-16 byte alignments before folding bytes for ANSI.
	if m.ascii {
		for i := 0; i+len(m.wide) <= len(data); i++ {
			match := true
			for j := 0; j < len(m.wide); j += 2 {
				c := data[i+j]
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				if c != m.wide[j] || data[i+j+1] != 0 {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	} else {
		for offset := range 2 {
			u := make([]uint16, max(0, (len(data)-offset)/2))
			for i := range u {
				u[i] = uint16(data[offset+2*i]) | uint16(data[offset+2*i+1])<<8
			}
			if containsFold(string(utf16.Decode(u)), string(m.ansi)) {
				return true
			}
		}
	}
	if !m.ascii {
		return bytes.Contains(bytes.ToLower(data), m.ansi)
	}
	for i, c := range data {
		if c >= 'A' && c <= 'Z' {
			data[i] = c + ('a' - 'A')
		}
	}
	return len(m.ansi) > 0 && bytes.Contains(data, m.ansi)
}

func (s *Scanner) scanRegistry(t *tokens, f *found) {
	if s.cancelled() {
		return
	}
	a := s.App
	if a.StillInstalled() {
		f.add(Leftover{Kind: KindKey, Hive: a.Hive, Path: a.KeyPath, Reason: "The app's entry in the list of installed apps", Sure: a.Broken})
	}
	for _, loc := range s.NameKeys {
		s.scanKeys(loc, t, f)
	}
	dir := t.installDir
	prefix := ""
	if dir != "" && depth(dir) >= 2 {
		prefix = strings.ToLower(dir + `\`)
	}
	mentionsDir := func(v string) bool { return prefix != "" && strings.Contains(strings.ToLower(v), prefix) }

	for _, loc := range s.RunKeys {
		if s.cancelled() {
			return
		}
		k, err := registry.OpenKey(loc.hive, loc.path, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		names, _ := k.ReadValueNames(-1)
		for _, name := range names {
			data, _, err := k.GetStringValue(name)
			if err != nil {
				continue
			}
			sure, maybe, _ := t.match(name)
			inDir := mentionsDir(data)
			if inDir || ((sure || maybe) && !commandExists(data)) {
				f.add(Leftover{Kind: KindValue, Hive: loc.hive, Path: loc.path, Value: name, Reason: "Starts with Windows", Sure: inDir || sure})
				approved := `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
				if hasValue(loc.hive, approved, name) {
					f.add(Leftover{Kind: KindValue, Hive: loc.hive, Path: approved, Value: name, Reason: "Startup setting", Sure: true})
				}
			}
		}
		k.Close()
	}
	for _, loc := range s.AppPathKeys {
		forSubkeys(loc, func(name string, k registry.Key) {
			target := iconPath(str(k, ""))
			if mentionsDir(target) || (t.exe != "" && strings.EqualFold(name, t.exe) && target != "" && !exists(target)) {
				f.add(Leftover{Kind: KindKey, Hive: loc.hive, Path: loc.path + `\` + name, Reason: "Program path", Sure: true})
			}
		})
	}
	if t.exe != "" {
		for _, loc := range s.AppKeys {
			path := loc.path + `\` + t.exe
			if keyExists(loc.hive, path) {
				gone := a.DisplayIcon == "" || !exists(a.DisplayIcon)
				f.add(Leftover{Kind: KindKey, Hive: loc.hive, Path: path, Reason: "File associations of the app's program", Sure: gone})
			}
		}
	}
	if prefix != "" {
		for _, loc := range s.MuiKeys {
			k, err := registry.OpenKey(loc.hive, loc.path, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			names, _ := k.ReadValueNames(-1)
			k.Close()
			for _, name := range names {
				if mentionsDir(name) {
					f.add(Leftover{Kind: KindValue, Hive: loc.hive, Path: loc.path, Value: name, Reason: "Program name Explorer remembers", Sure: true})
				}
			}
		}
	}
}

// scanKeys looks for the app's keys under loc, as scanDir looks for its
// folders.
func (s *Scanner) scanKeys(loc regLoc, t *tokens, f *found) {
	if s.cancelled() {
		return
	}
	k, err := registry.OpenKey(loc.hive, loc.path, registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if err != nil {
		return
	}
	names, _ := k.ReadSubKeyNames(-1)
	k.Close()
	for _, name := range names {
		if s.cancelled() {
			return
		}
		path := loc.path + `\` + name
		if sure, maybe, other := t.match(name); sure || maybe {
			f.add(Leftover{Kind: KindKey, Hive: loc.hive, Path: path, Reason: reason("Settings key named after the app", other, true), Sure: sure})
			continue
		}
		if t.publisher == "" || norm(name) != t.publisher {
			continue
		}
		pk, err := registry.OpenKey(loc.hive, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		children, _ := pk.ReadSubKeyNames(-1)
		info, _ := pk.Stat()
		pk.Close()
		var mine []Leftover
		all := len(children) > 0 || (info != nil && info.ValueCount == 0)
		for _, c := range children {
			sure, maybe, other := t.match(c, true)
			if !sure {
				all = false
			}
			if sure || maybe {
				mine = append(mine, Leftover{Kind: KindKey, Hive: loc.hive, Path: path + `\` + c, Reason: reason("Under the publisher's key", other, true), Sure: sure})
			}
		}
		if all && !protectedNames[t.publisher] {
			f.add(Leftover{Kind: KindKey, Hive: loc.hive, Path: path, Reason: "Publisher's key, holding only the app's settings", Sure: true})
			continue
		}
		for _, l := range mine {
			f.add(l)
		}
	}
}

func forSubkeys(loc regLoc, fn func(name string, k registry.Key)) {
	k, err := registry.OpenKey(loc.hive, loc.path, registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if err != nil {
		return
	}
	names, _ := k.ReadSubKeyNames(-1)
	k.Close()
	for _, name := range names {
		sk, err := registry.OpenKey(loc.hive, loc.path+`\`+name, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		fn(name, sk)
		sk.Close()
	}
}

func keyExists(hive registry.Key, path string) bool {
	k, err := registry.OpenKey(hive, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

func hasValue(hive registry.Key, path, name string) bool {
	k, err := registry.OpenKey(hive, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetValue(name, nil)
	return err == nil
}

// removable reports whether a folder may be removed: it is none of the
// system's or the user's own folders, nor holds them, nor holds an app still
// installed, nor this app.
func (s *Scanner) removable(path string) bool {
	p := lowerPath(path)
	if depth(p) < 1 || protectedNames[norm(filepath.Base(p))] {
		return false
	}
	for q := range s.Protected {
		if p == q || strings.HasPrefix(q, p+`\`) {
			return false
		}
	}
	if s.self != "" && (p == s.self || strings.HasPrefix(s.self, p+`\`)) {
		return false
	}
	for _, d := range s.otherDirs {
		if d == p || strings.HasPrefix(d, p+`\`) {
			return false
		}
	}
	return true
}

// pruneNested drops the leftovers inside a folder or key that goes as a
// whole, unless they are surer than it is.
func pruneNested(items *[]Leftover) {
	var nFolders, nKeys int
	for _, l := range *items {
		if l.Kind == KindFolder {
			nFolders++
		}
		if l.Kind == KindKey {
			nKeys++
		}
	}
	folders, keys := make(map[string]bool, nFolders), make(map[string]bool, nKeys)
	for _, l := range *items {
		switch l.Kind {
		case KindFolder:
			p := lowerPath(l.Path)
			folders[p] = folders[p] || l.Sure
		case KindKey:
			p := strings.ToLower(l.Display())
			keys[p] = keys[p] || l.Sure
		}
	}
	inside := func(p string, sure bool, in map[string]bool) bool {
		for {
			i := strings.LastIndexByte(p, '\\')
			if i < 0 {
				return false
			}
			p = p[:i]
			if parentSure, ok := in[p]; ok && (parentSure || !sure) {
				return true
			}
		}
	}
	*items = slices.DeleteFunc(*items, func(l Leftover) bool {
		switch l.Kind {
		case KindFolder, KindFile:
			return inside(lowerPath(l.Path), l.Sure, folders)
		case KindKey:
			return inside(strings.ToLower(l.Display()), l.Sure, keys)
		}
		return false
	})
}

// measure sets the size of the files and folders, walking the folders in
// parallel.
func measure(items []Leftover) {
	measureContext(context.Background(), items, nil)
}

func diskSize(path string) int64 {
	return diskSizeContext(context.Background(), path)
}

func (s *Scanner) cancelled() bool { return s.ctx != nil && s.ctx.Err() != nil }

// appInstallDir is where an app is installed: its InstallLocation, or the
// folder of its program or uninstaller.
func appInstallDir(a *App) string {
	if a.InstallLocation != "" {
		return a.InstallLocation
	}
	for i := range 2 {
		p := a.DisplayIcon
		if i == 1 {
			p = commandExe(a.UninstallString)
		}
		ext := strings.ToLower(filepath.Ext(p))
		if p == "" || (ext != ".exe" && ext != ".ico") || !filepath.IsAbs(p) {
			continue
		}
		d := filepath.Dir(p)
		if b := norm(filepath.Base(d)); b == "uninstall" || b == "uninst" || b == "uninstaller" || b == "bin" {
			d = filepath.Dir(d)
		}
		l := lowerPath(d)
		if strings.HasPrefix(l, lowerPath(os.Getenv("SystemRoot"))) || strings.Contains(l, `\package cache\`) ||
			strings.Contains(l, `\temp\`) || strings.HasSuffix(l, `\temp`) || depth(l) < 2 {
			continue
		}
		// A program's folder is the app's only when named after it: a
		// game's icon may sit in its launcher's folder, which holds others.
		b, n := norm(cleanName(filepath.Base(d))), norm(cleanName(a.Name))
		if len(b) < 3 || !(strings.Contains(n, b) || strings.Contains(b, n)) {
			continue
		}
		return d
	}
	return ""
}

// mainExe is the file name of an app's program, from its icon, lower case.
func mainExe(a *App) string {
	if !strings.EqualFold(filepath.Ext(a.DisplayIcon), ".exe") {
		return ""
	}
	base := strings.ToLower(filepath.Base(a.DisplayIcon))
	for _, prefix := range []string{"unins", "uninst", "setup", "install", "update", "msiexec", "rundll32", "helper", "maintenancetool"} {
		if strings.HasPrefix(base, prefix) {
			return ""
		}
	}
	return base
}

var (
	versionRe = regexp.MustCompile(`(?i)[\s\-_]*[\(\[]?\b(v(ersion)?\s*)?\d+(\.\d+)+[a-z0-9.\-+]*[\)\]]?`)
	archRe    = regexp.MustCompile(`(?i)[\s\-_]*[\(\[]?\b(x64|x86|x86_64|amd64|arm64|64[- ]?bit|32[- ]?bit|win64|win32)\b[\)\]]?`)
	parenRe   = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]\s*$`)
	localeRe  = regexp.MustCompile(`(?i)\s+-\s+[a-z]{2}(-[a-z]{2,4})?\s*$`)
	yearRe    = regexp.MustCompile(`\s+(19|20)\d\d$`)
)

// cleanName takes the version, architecture, language and editions in
// parentheses out of an app's name: "Mozilla Firefox (x64 en-US)" is
// "Mozilla Firefox".
func cleanName(name string) string {
	s := strings.TrimSpace(name)
	for parenRe.MatchString(s) {
		s = parenRe.ReplaceAllString(s, "")
	}
	s = localeRe.ReplaceAllString(s, "")
	s = archRe.ReplaceAllString(s, "")
	s = versionRe.ReplaceAllString(s, "")
	s = yearRe.ReplaceAllString(s, "")
	return strings.Trim(s, " -_.,")
}

var publisherSuffixRe = regexp.MustCompile(`(?i)[\s,.]+(inc|incorporated|llc|ltd|limited|corp|corporation|co|company|gmbh|ag|s\.?a|s\.?r\.?l|b\.?v|pty|plc|software|technologies|technology|team|contributors|foundation|project|group|labs)\.?$`)

func cleanPublisher(p string) string {
	p = strings.TrimSpace(p)
	for {
		q := publisherSuffixRe.ReplaceAllString(p, "")
		if q == p {
			break
		}
		p = q
	}
	return strings.TrimPrefix(p, "The ")
}

// norm is a name folded for comparing: lower case, letters, digits, + and
// # only, so that "Notepad++", "notepad++" and "Notepad ++" are one name.
func norm(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '#' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return norm(f[0])
	}
	return ""
}

// genericNames are names too common to say whose folder or key it is.
var genericNames = map[string]bool{
	"app": true, "apps": true, "application": true, "applications": true, "bin": true, "data": true,
	"cache": true, "config": true, "settings": true, "update": true, "updater": true, "uninstall": true,
	"setup": true, "installer": true, "launcher": true, "tools": true, "tool": true, "driver": true,
	"drivers": true, "runtime": true, "service": true, "services": true, "software": true, "user": true,
	"users": true, "default": true, "local": true, "roaming": true, "temp": true, "tmp": true,
	"logs": true, "log": true, "plugins": true, "client": true, "desktop": true, "documents": true,
	"helper": true, "common": true, "shared": true, "system": true, "program": true, "programs": true,
	"games": true, "game": true, "free": true, "pro": true, "home": true, "lite": true, "the": true,
	"new": true, "my": true, "win": true, "windows": true, "edition": true, "update64": true,
}

// protectedNames are folders and keys of Windows and of vendors shared by
// many apps, which are never removed whole.
var protectedNames = map[string]bool{
	"microsoft": true, "microsoftcorporation": true, "windows": true, "windowsapps": true,
	"windowsnt": true, "windowsdefender": true, "windowsmail": true, "windowsmediaplayer": true,
	"windowsphotoviewer": true, "windowspowershell": true, "windowssidebar": true,
	"commonfiles": true, "microsoftshared": true, "internetexplorer": true, "classes": true,
	"clients": true, "policies": true, "wow6432node": true, "registeredapplications": true,
	"odbc": true, "intel": true, "nvidia": true, "nvidiacorporation": true, "amd": true,
	"realtek": true, "packages": true, "package cache": true, "packagecache": true, "installer": true,
	"system32": true, "syswow64": true, "programdata": true, "programfiles": true, "programfilesx86": true,
	"appdata": true, "users": true, "public": true, "default": true, "temp": true, "startmenu": true,
	"onedrive": true, "google": true, "mozilla": true, "adobe": true, "apple": true, "appleinc": true,
	"java": true, "oracle": true, "dotnet": true, "nuget": true, "steam": true, "steamapps": true,
	"desktop": true, "documents": true, "downloads": true, "pictures": true, "music": true, "videos": true,
}

func lowerPath(p string) string {
	if p == "" {
		return ""
	}
	return strings.ToLower(strings.TrimRight(filepath.Clean(p), `\`))
}

// depth is how many folders deep a path is below its drive: C:\ is 0,
// C:\Games 1.
func depth(p string) int {
	p = strings.TrimRight(filepath.Clean(p), `\`)
	v := filepath.VolumeName(p)
	rest := strings.Trim(p[len(v):], `\`)
	if rest == "" {
		return 0
	}
	return strings.Count(rest, `\`) + 1
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func containsFold(s, sub string) bool {
	return sub != "" && strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
