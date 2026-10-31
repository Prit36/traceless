package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/egoist/mygo"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Failure is a leftover that could not be removed, and why.
type Failure struct {
	Item Leftover
	Err  error
}

// CleanResult is what removing leftovers did.
type CleanResult struct {
	Removed   int
	Freed     int64 // bytes of files removed
	Failed    []Failure
	BackupDir string // where the registry was backed up, if it was
}

// Trash removes files to the Recycle Bin, unless permanent; Clean uses it so
// that tests can do without the shell.
var Trash = func(path string) error { return mygo.Shell.TrashItem(path) }

// Clean removes leftovers: registry keys and values once they are saved to
// .reg files in a new folder of backupRoot, then files and folders, to the
// Recycle Bin or for good.
func Clean(items []Leftover, appName, backupRoot string, permanent bool) CleanResult {
	var res CleanResult
	var values []Leftover
	for _, l := range items {
		if l.IsRegistry() && res.BackupDir == "" {
			res.BackupDir = filepath.Join(backupRoot, fsName(appName)+" "+time.Now().Format("2006-01-02 150405"))
			if err := os.MkdirAll(res.BackupDir, 0o755); err != nil {
				res.BackupDir = ""
				for _, l := range items {
					if l.IsRegistry() {
						res.Failed = append(res.Failed, Failure{l, fmt.Errorf("could not back it up: %w", err)})
					}
				}
				break
			}
		}
		if l.Kind == KindValue {
			values = append(values, l)
		}
	}
	if res.BackupDir != "" && len(values) > 0 {
		if err := exportValues(values, filepath.Join(res.BackupDir, "values.reg")); err != nil {
			for _, l := range values {
				res.Failed = append(res.Failed, Failure{l, fmt.Errorf("could not back it up: %w", err)})
			}
		}
	}

	n := 0
	for _, l := range items {
		var err error
		switch l.Kind {
		case KindValue:
			if res.BackupDir == "" || failed(res.Failed, l) {
				continue
			}
			err = deleteValue(l)
		case KindKey:
			if res.BackupDir == "" {
				continue
			}
			n++
			file := filepath.Join(res.BackupDir, fmt.Sprintf("%02d %s.reg", n, fsName(filepath.Base(l.Path))))
			if err = exportKey(l, file); err != nil {
				err = fmt.Errorf("could not back it up: %w", err)
			} else {
				err = deleteKeyTree(l.Hive, l.Path)
			}
		default:
			if permanent {
				err = removeAll(l.Path)
			} else {
				err = Trash(l.Path)
			}
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, registry.ErrNotExist) {
			err = nil // already gone
		}
		if err != nil {
			res.Failed = append(res.Failed, Failure{l, err})
			continue
		}
		res.Removed++
		res.Freed += l.Size
	}
	return res
}

func failed(fs []Failure, l Leftover) bool {
	for _, f := range fs {
		if f.Item.Display() == l.Display() {
			return true
		}
	}
	return false
}

// exportKey saves a key and its subkeys as reg.exe does, which the user
// can open to put them back.
func exportKey(l Leftover, file string) error {
	cmd := exec.Command("reg.exe", "export", hiveName(l.Hive)+`\`+l.Path, file, "/y", "/reg:64")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		if !keyExists(l.Hive, l.Path) {
			return registry.ErrNotExist
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// exportValues writes values to a .reg file, each in hex of its own type,
// which regedit reads back as it was.
func exportValues(values []Leftover, file string) error {
	var b strings.Builder
	b.WriteString("Windows Registry Editor Version 5.00\r\n")
	last := ""
	for _, l := range values {
		k, err := registry.OpenKey(l.Hive, l.Path, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		size, typ, err := k.GetValue(l.Value, nil)
		var data []byte
		if err == nil && size > 0 {
			data = make([]byte, size)
			_, typ, err = k.GetValue(l.Value, data)
		}
		k.Close()
		if err != nil {
			continue
		}
		if key := hiveName(l.Hive) + `\` + l.Path; key != last {
			fmt.Fprintf(&b, "\r\n[%s]\r\n", key)
			last = key
		}
		name := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(l.Value)
		if l.Value == "" {
			b.WriteString("@=")
		} else {
			fmt.Fprintf(&b, "\"%s\"=", name)
		}
		if typ == registry.DWORD && len(data) == 4 {
			fmt.Fprintf(&b, "dword:%08x\r\n", uint32(data[0])|uint32(data[1])<<8|uint32(data[2])<<16|uint32(data[3])<<24)
			continue
		}
		if typ == registry.BINARY {
			b.WriteString("hex:")
		} else {
			fmt.Fprintf(&b, "hex(%x):", typ)
		}
		for i, c := range data {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%02x", c)
		}
		b.WriteString("\r\n")
	}
	// regedit reads .reg files in UTF-16 with a byte order mark.
	u := utf16.Encode([]rune(b.String()))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return os.WriteFile(file, out, 0o644)
}

func deleteValue(l Leftover) error {
	k, err := registry.OpenKey(l.Hive, l.Path, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.DeleteValue(l.Value)
}

var procRegDeleteTree = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegDeleteTreeW")

// deleteKeyTree deletes a key with its subkeys and values.
func deleteKeyTree(hive registry.Key, path string) error {
	parent, name := filepath.Split(path)
	parent = strings.TrimRight(parent, `\`)
	pk, err := registry.OpenKey(hive, parent, registry.ALL_ACCESS|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer pk.Close()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	if rc, _, _ := procRegDeleteTree.Call(uintptr(pk), uintptr(unsafe.Pointer(p))); rc != 0 {
		return syscall.Errno(rc)
	}
	// RegDeleteTree empties the key; take the key itself away too.
	if err := registry.DeleteKey(pk, name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// removeAll deletes a file or folder for good, read-only files too.
func removeAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			os.Chmod(p, 0o666)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// fsName makes a string a file name.
func fsName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimRight(strings.TrimSpace(s), ".")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "app"
	}
	return s
}
