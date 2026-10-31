package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestPruneNestedCertaintyAndHives(t *testing.T) {
	items := []Leftover{
		{Kind: KindFolder, Path: `C:\App`, Sure: false},
		{Kind: KindFolder, Path: `C:\App\Certain`, Sure: true},
		{Kind: KindFile, Path: `c:\APP\maybe.txt`},
		{Kind: KindFile, Path: `C:\Application\keep.txt`},
		{Kind: KindKey, Hive: registry.CURRENT_USER, Path: `Software\App`, Sure: true},
		{Kind: KindKey, Hive: registry.CURRENT_USER, Path: `SOFTWARE\APP\Child`, Sure: true},
		{Kind: KindKey, Hive: registry.LOCAL_MACHINE, Path: `Software\App\Child`, Sure: true},
	}
	pruneNested(&items)
	if len(items) != 5 {
		t.Fatalf("pruned: %+v", items)
	}
	for _, l := range items {
		if strings.EqualFold(l.Path, `C:\App\maybe.txt`) || l.Hive == registry.CURRENT_USER && strings.HasSuffix(l.Path, `\Child`) {
			t.Fatalf("nested item retained: %+v", l)
		}
	}
}

func TestPruneNestedMatchesReference(t *testing.T) {
	var base []Leftover
	for i := range 80 {
		for level := range 4 {
			path := fmt.Sprintf(`C:\Apps\App%d%s`, i, strings.Repeat(`\Child`, level))
			base = append(base, Leftover{Kind: KindFolder, Path: path, Sure: (i+level)%3 == 0})
			base = append(base, Leftover{Kind: KindFile, Path: path + `\file.txt`, Sure: (i+level)%2 == 0})
			base = append(base, Leftover{Kind: KindKey, Hive: registry.CURRENT_USER, Path: `Software\` + path[3:], Sure: (i+level)%3 == 0})
		}
	}
	want := slices.DeleteFunc(slices.Clone(base), func(l Leftover) bool {
		for _, parent := range base {
			if !parent.Sure && l.Sure {
				continue
			}
			if (l.Kind == KindFolder || l.Kind == KindFile) && parent.Kind == KindFolder && strings.HasPrefix(lowerPath(l.Path), lowerPath(parent.Path)+`\`) {
				return true
			}
			if l.Kind == KindKey && parent.Kind == KindKey && strings.HasPrefix(strings.ToLower(l.Display()), strings.ToLower(parent.Display())+`\`) {
				return true
			}
		}
		return false
	})
	got := slices.Clone(base)
	pruneNested(&got)
	if !slices.Equal(got, want) {
		t.Fatalf("pruned results differ: got %d, want %d", len(got), len(want))
	}
}

func TestDiscoverBeforeMeasurementAndRepeat(t *testing.T) {
	f := newFakeSystem(t)
	dir := filepath.Join(f.programs, "FakeApp")
	f.file(`Program Files\FakeApp\data.bin`, make([]byte, 4096))
	app := fakeApp("FakeApp", "Fake Vendor", dir)
	s := f.scanner(app, fakeApp("OtherApp", "Other Vendor", filepath.Join(f.programs, "OtherApp")))
	items := s.Discover(context.Background())
	if len(items) != 1 || items[0].Size != 0 || !items[0].Selected {
		t.Fatalf("discovery: %+v", items)
	}
	for range 3 {
		got := s.Scan()
		if len(got) != 1 || got[0].Size != 4096 || len(s.otherDirs) != 1 {
			t.Fatalf("repeat scan: %+v, dirs %v", got, s.otherDirs)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.Discover(ctx); len(got) != 0 {
		t.Fatalf("cancelled discovery: %+v", got)
	}
	if got := diskSizeContext(ctx, dir); got != 0 {
		t.Fatalf("cancelled size: %d", got)
	}
}

func TestMeasureCancellationAndBoundedWorkers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(file, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	items := make([]Leftover, 10000)
	for i := range items {
		items[i] = Leftover{Kind: KindFile, Path: file}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := runtime.NumGoroutine()
	n := 0
	measureContext(ctx, items, func(u sizeUpdate) {
		n++
		if n == 1 {
			if got := runtime.NumGoroutine() - before; got > 10 {
				t.Errorf("unbounded workers: %d", got)
			}
			cancel()
		}
	})
	if n != 1 {
		t.Fatalf("callbacks after cancellation: %d", n)
	}
}

func TestDiskSizeLongPathsAndLinks(t *testing.T) {
	root := t.TempDir()
	dir := root
	for range 8 {
		dir = filepath.Join(dir, strings.Repeat("long", 12))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "data")
	if err := os.WriteFile(file, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := diskSize(root); got != 8 {
		t.Fatalf("long path size: %d", got)
	}
	if got := diskSize(file); got != 8 {
		t.Fatalf("file size: %d", got)
	}
	if got := diskSize(filepath.Join(root, "missing")); got != 0 {
		t.Fatalf("missing size: %d", got)
	}
	link := filepath.Join(root, "loop")
	if err := os.Symlink(root, link); err != nil {
		t.Logf("symlink unavailable: %v", err)
		return
	}
	if got := diskSize(root); got != 8 {
		t.Fatalf("followed symlink: %d", got)
	}
	if got := diskSize(link); got != 0 {
		t.Fatalf("followed root symlink: %d", got)
	}
}

func TestShortcutEncodings(t *testing.T) {
	for _, path := range []string{`C:\Program Files\Example\`, `C:\Apps\Éditeur\`, `C:\Apps\工具\`} {
		for _, wide := range []bool{false, true} {
			for offset := range 2 {
				t.Run(fmt.Sprintf("%s/wide=%v/offset=%d", path, wide, offset), func(t *testing.T) {
					file := filepath.Join(t.TempDir(), "shortcut.lnk")
					data := []byte(strings.ToUpper(path) + "program.exe")
					if wide {
						data = utf16le(strings.ToUpper(path) + "program.exe")
					}
					data = append(make([]byte, offset), data...)
					if err := os.WriteFile(file, data, 0o600); err != nil {
						t.Fatal(err)
					}
					if !fileMentions(file, path) {
						t.Fatal("target missed")
					}
					if fileMentions(file, path+"Other\\") {
						t.Fatal("unrelated target matched")
					}
				})
			}
		}
	}
}

func TestIconCacheBoundedAndRecentlyUsed(t *testing.T) {
	m := newModel(false)
	m.cacheIcon("keep", nil)
	for i := range 1000 {
		m.icon(&App{DisplayIcon: "keep"})
		m.cacheIcon(fmt.Sprint(i), nil)
	}
	if len(m.icons) != 128 {
		t.Fatalf("cache size: %d", len(m.icons))
	}
	if _, ok := m.icons["keep"]; !ok {
		t.Fatal("recently used icon evicted")
	}
	m.shutdown()
	m.shutdown()
}

func TestNativeIconExtraction(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initIconApartment(); err != nil {
		t.Fatal(err)
	}
	defer windows.CoUninitialize()
	path := filepath.Join(os.Getenv("SystemRoot"), "System32", "notepad.exe")
	for _, size := range []int{20, 32, 64} {
		icon, err := loadFileIcon(path, size)
		if err != nil {
			t.Fatal(err)
		}
		w, h := icon.Size()
		if w <= 0 || h <= 0 {
			t.Fatalf("icon dimensions: %dx%d", w, h)
		}
	}
	if _, err := loadFileIcon(filepath.Join(t.TempDir(), "missing.exe"), 32); err == nil {
		t.Fatal("missing icon succeeded")
	}
}

func TestFilterClearsHiddenReferencesAndUpdatesCounts(t *testing.T) {
	m := newModel(false)
	m.apps = filterTestApps(2000)
	m.filter()
	old := m.shown
	m.query = "application 01999"
	m.filter()
	if len(m.shown) != 1 || slices.ContainsFunc(old[1:], func(a *App) bool { return a != nil }) {
		t.Fatal("hidden app references retained")
	}
	m.apps = filterTestApps(2)
	m.query = ""
	m.filter()
	if m.counts["all"] != 2 {
		t.Fatalf("stale counts: %v", m.counts)
	}
}

func filterTestApps(n int) []*App {
	apps := make([]*App, n)
	for i := range apps {
		name := fmt.Sprintf("Application %05d", i)
		apps[i] = &App{Name: name, lowerName: strings.ToLower(name), id: name}
	}
	return apps
}

func TestLogoSupportsHighDPI(t *testing.T) {
	for _, px := range []int{16, 20, 24, 32, 64, 96} {
		b := logo(px)
		w, h := b.Size()
		if w != px || h != px {
			t.Fatalf("logo: %dx%d", w, h)
		}
	}
	if logo(0) != nil {
		t.Fatal("invalid logo size")
	}
	// Exercise a real bitmap in the cache as well as negative lookups.
	m := newModel(false)
	m.cacheIcon("bitmap", ui.NewBitmap(image.NewRGBA(image.Rect(0, 0, 32, 32))))
}
