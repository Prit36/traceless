package main

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

// App is a program Windows lists as installed: one key of an Uninstall
// registry branch.
type App struct {
	Name            string
	Version         string
	Publisher       string
	InstallLocation string
	InstallDate     time.Time
	SizeKB          uint64
	UninstallString string
	QuietUninstall  string
	DisplayIcon     string // the path of its icon, without ",index"
	MSI             bool   // a Windows Installer product, removed by msiexec
	ProductCode     string // the key's name when it is an MSI product code
	Dir             string // where it is installed, as far as can be told

	Hive     registry.Key // registry.LOCAL_MACHINE or registry.CURRENT_USER
	KeyPath  string       // the Uninstall key, under Hive, in the 64-bit view
	AllUsers bool
	Is32     bool

	// Broken is set when the uninstaller the entry points to is missing:
	// the app can only be removed by cleaning up what is left of it.
	Broken bool

	id, lowerName, lowerPublisher string
	sizeText, dateText            string
	dirExists                     bool
}

// ID identifies the app's registry entry.
func (a *App) ID() string {
	if a.id == "" {
		a.id = hiveName(a.Hive) + `\` + a.KeyPath
	}
	return a.id
}

// CanQuiet reports whether the app can be removed without the
// uninstaller's own windows.
func (a *App) CanQuiet() bool { return a.QuietUninstall != "" || a.ProductCode != "" }

// Command is the command line that removes the app.
func (a *App) Command(quiet bool) string {
	if a.ProductCode != "" && (a.MSI || isMsiexec(a.UninstallString)) {
		cmd := "msiexec.exe /x " + a.ProductCode
		if quiet {
			cmd += " /passive /norestart"
		}
		return cmd
	}
	if quiet && a.QuietUninstall != "" {
		return a.QuietUninstall
	}
	return a.UninstallString
}

// StillInstalled reports whether the app's registry entry is still there.
func (a *App) StillInstalled() bool {
	k, err := registry.OpenKey(a.Hive, a.KeyPath, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// uninstallBranches are where Windows keeps the installed programs: for all
// users, 64- and 32-bit, and for the current user.
var uninstallBranches = []struct {
	hive     registry.Key
	path     string
	allUsers bool
	is32     bool
}{
	{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, true, false},
	{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`, true, true},
	{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall`, false, false},
	{registry.CURRENT_USER, `Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`, false, true},
}

// LoadApps reads the installed programs from the registry, the branches in
// parallel, leaving out updates, system components and duplicates.
func LoadApps() []*App {
	results := make([][]*App, len(uninstallBranches))
	var wg sync.WaitGroup
	for i, b := range uninstallBranches {
		wg.Go(func() {
			results[i] = readBranch(b.hive, b.path, b.allUsers, b.is32)
		})
	}
	wg.Wait()

	seen := make(map[string]bool)
	var apps []*App
	for _, list := range results {
		for _, a := range list {
			key := a.lowerName + "\x00" + a.Version + "\x00" + a.lowerPublisher
			if seen[key] {
				continue
			}
			seen[key] = true
			apps = append(apps, a)
		}
	}
	slices.SortFunc(apps, func(a, b *App) int { return cmp.Compare(a.lowerName, b.lowerName) })
	return apps
}

func readBranch(hive registry.Key, path string, allUsers, is32 bool) []*App {
	branch, err := registry.OpenKey(hive, path, registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if err != nil {
		return nil
	}
	defer branch.Close()
	names, _ := branch.ReadSubKeyNames(-1)
	apps := make([]*App, len(names))
	var wg sync.WaitGroup
	// At most eight registry readers across the four branches. Two per
	// branch keeps an uneven registry from serializing the entire load.
	for worker := range min(2, len(names)) {
		wg.Go(func() {
			for i := worker; i < len(names); i += 2 {
				if a := readApp(hive, path+`\`+names[i], names[i]); a != nil {
					a.AllUsers, a.Is32 = allUsers, is32
					apps[i] = a
				}
			}
		})
	}
	wg.Wait()
	valid := apps[:0]
	for _, a := range apps {
		if a != nil {
			valid = append(valid, a)
		}
	}
	clear(apps[len(valid):])
	return valid
}

var productCodeRe = regexp.MustCompile(`^\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}$`)

func readApp(hive registry.Key, path, keyName string) *App {
	k, err := registry.OpenKey(hive, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return nil
	}
	defer k.Close()

	name := str(k, "DisplayName")
	if name == "" || num(k, "SystemComponent") == 1 || str(k, "ParentKeyName") != "" {
		return nil
	}
	switch strings.ToLower(str(k, "ReleaseType")) {
	case "update", "hotfix", "security update", "service pack":
		return nil
	}
	a := &App{
		Name:            name,
		Version:         str(k, "DisplayVersion"),
		Publisher:       str(k, "Publisher"),
		InstallLocation: cleanPath(str(k, "InstallLocation")),
		SizeKB:          num(k, "EstimatedSize"),
		UninstallString: str(k, "UninstallString"),
		QuietUninstall:  str(k, "QuietUninstallString"),
		DisplayIcon:     iconPath(str(k, "DisplayIcon")),
		MSI:             num(k, "WindowsInstaller") == 1,
		Hive:            hive,
		KeyPath:         path,
	}
	if productCodeRe.MatchString(keyName) {
		a.ProductCode = keyName
	}
	if a.UninstallString == "" && a.ProductCode == "" {
		return nil // nothing can remove it
	}
	a.InstallDate = parseInstallDate(str(k, "InstallDate"))
	if a.InstallDate.IsZero() {
		if info, err := k.Stat(); err == nil {
			a.InstallDate = info.ModTime()
		}
	}
	a.prepare()
	return a
}

// prepare works out what the app's fields imply: where it is installed and
// whether its uninstaller is there.
func (a *App) prepare() {
	a.Dir = appInstallDir(a)
	a.dirExists = a.Dir != "" && exists(a.Dir)
	a.Broken = !(a.ProductCode != "" && (a.MSI || isMsiexec(a.UninstallString))) && !commandExists(a.UninstallString)
	a.lowerName, a.lowerPublisher = strings.ToLower(a.Name), strings.ToLower(a.Publisher)
	a.sizeText, a.dateText = formatBytes(int64(a.SizeKB)*1024), formatDate(a.InstallDate)
	a.ID()
}

func str(k registry.Key, name string) string {
	s, typ, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	if typ == registry.EXPAND_SZ {
		if x, err := registry.ExpandString(s); err == nil {
			s = x
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
}

func num(k registry.Key, name string) uint64 {
	n, _, err := k.GetIntegerValue(name)
	if err != nil {
		if s := str(k, name); s != "" {
			n, _ = strconv.ParseUint(s, 10, 64)
		}
	}
	return n
}

func parseInstallDate(s string) time.Time {
	for _, layout := range []string{"20060102", "2006-01-02", "1/2/2006", "01/02/2006", "2006/01/02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil && t.Year() > 1990 {
			return t
		}
	}
	return time.Time{}
}

// cleanPath trims quotes and a trailing separator from a path the registry
// holds.
func cleanPath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), `"`)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if len(p) > 3 {
		p = strings.TrimRight(p, `\`)
	}
	return p
}

// iconPath turns a DisplayIcon value, as `"C:\App\app.exe",0`, into the
// file's path.
func iconPath(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) {
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return cleanPath(v[1 : end+1])
		}
	}
	if i := strings.LastIndexByte(v, ','); i > 0 {
		if _, err := strconv.Atoi(strings.TrimSpace(v[i+1:])); err == nil {
			v = v[:i]
		}
	}
	return cleanPath(v)
}

func isMsiexec(cmd string) bool {
	return strings.Contains(strings.ToLower(cmd), "msiexec")
}

// commandExe finds the program a command line runs, as CreateProcess does
// for one whose path is not quoted: the shortest prefix ending at a space
// that names a file.
func commandExe(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
		return strings.Trim(cmd, `"`)
	}
	for i := 0; i <= len(cmd); i++ {
		if i == len(cmd) || cmd[i] == ' ' {
			p := cmd[:i]
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
			if !strings.ContainsAny(p, `\/`) || filepath.Ext(p) == "" {
				if q, err := lookPath(p); err == nil {
					return q
				}
			}
		}
	}
	if i := strings.IndexByte(cmd, ' '); i > 0 {
		return cmd[:i]
	}
	return cmd
}

// commandArgs returns what follows the program in a command line.
func commandArgs(cmd, exe string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		cmd = cmd[min(len(cmd), len(exe)+2):]
	} else if strings.HasPrefix(strings.ToLower(cmd), strings.ToLower(exe)) {
		cmd = cmd[len(exe):]
	} else if i := strings.IndexByte(cmd, ' '); i > 0 {
		cmd = cmd[i:]
	} else {
		cmd = ""
	}
	return strings.TrimSpace(cmd)
}

func commandExists(cmd string) bool {
	exe := commandExe(cmd)
	if exe == "" {
		return false
	}
	if st, err := os.Stat(exe); err == nil && !st.IsDir() {
		return true
	}
	_, err := lookPath(exe)
	return err == nil
}

// lookPath finds a program in the system directories, as rundll32.exe or
// msiexec, without searching the current directory.
func lookPath(name string) (string, error) {
	if filepath.Ext(name) == "" {
		name += ".exe"
	}
	for _, dir := range []string{os.Getenv("SystemRoot") + `\System32`, os.Getenv("SystemRoot")} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func hiveName(k registry.Key) string {
	switch k {
	case registry.LOCAL_MACHINE:
		return "HKEY_LOCAL_MACHINE"
	case registry.CURRENT_USER:
		return "HKEY_CURRENT_USER"
	case registry.CLASSES_ROOT:
		return "HKEY_CLASSES_ROOT"
	case registry.USERS:
		return "HKEY_USERS"
	}
	return "HKEY"
}

func shortHiveName(k registry.Key) string {
	switch k {
	case registry.LOCAL_MACHINE:
		return "HKLM"
	case registry.CURRENT_USER:
		return "HKCU"
	case registry.CLASSES_ROOT:
		return "HKCR"
	case registry.USERS:
		return "HKU"
	}
	return "HKEY"
}
