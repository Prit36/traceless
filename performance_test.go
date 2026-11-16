package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"golang.org/x/sys/windows/registry"
)

func BenchmarkNewModel(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = newModel(false)
	}
}

func BenchmarkLoadApps(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		LoadApps()
	}
}

func benchmarkApps(n int) []*App {
	apps := make([]*App, n)
	for i := range apps {
		name := fmt.Sprintf("Application %05d", i)
		apps[i] = &App{Name: name, Publisher: "Example vendor", SizeKB: uint64(i * 1024),
			lowerName: strings.ToLower(name), lowerPublisher: "example vendor", id: name}
		apps[i].sizeText = formatBytes(int64(apps[i].SizeKB) * 1024)
	}
	return apps
}

func BenchmarkFilter(b *testing.B) {
	m := newModel(false)
	m.apps = benchmarkApps(2000)
	m.filter()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m.query = "application 01"
		m.filter()
	}
}

func BenchmarkViewFrame(b *testing.B) {
	m := newModel(false)
	m.apps, m.loading = benchmarkApps(2000), false
	m.filter()
	tt := ui.NewTester(m.view, 1060, 700)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tt.Frame()
	}
}

func BenchmarkPruneNested(b *testing.B) {
	base := make([]Leftover, 0, 2000)
	for i := range 1000 {
		path := fmt.Sprintf(`C:\Apps\Application%04d`, i)
		base = append(base, Leftover{Kind: KindFolder, Path: path, Sure: true},
			Leftover{Kind: KindFile, Path: path + `\data.bin`, Sure: true})
	}
	items := make([]Leftover, len(base))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		items = items[:len(base)]
		copy(items, base)
		pruneNested(&items)
	}
}

func benchmarkDiskTree(b *testing.B) string {
	b.Helper()
	root := b.TempDir()
	for i := range 20 {
		dir := filepath.Join(root, fmt.Sprint(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		for j := range 100 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(j)), []byte("benchmark contents"), 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	return root
}

func BenchmarkDiskSize(b *testing.B) {
	root := benchmarkDiskTree(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := diskSize(root); got != 20*100*int64(len("benchmark contents")) {
			b.Fatalf("size = %d", got)
		}
	}
}

func BenchmarkFileMentions(b *testing.B) {
	file := filepath.Join(b.TempDir(), "shortcut.lnk")
	data := append(make([]byte, 4096), utf16le(`C:\Program Files\Example\example.exe`)...)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !fileMentions(file, `C:\Program Files\Example\`) {
			b.Fatal("shortcut target not found")
		}
	}
}

func BenchmarkScanner(b *testing.B) {
	root := benchmarkDiskTree(b)
	dir := filepath.Join(root, "FakeApp")
	if err := os.Mkdir(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), make([]byte, 4096), 0o600); err != nil {
		b.Fatal(err)
	}
	app := fakeApp("FakeApp", "Fake Vendor", dir)
	s := &Scanner{App: app, Dirs: []string{root}, Protected: map[string]bool{lowerPath(root): true}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		items := s.Scan()
		if len(items) != 1 || items[0].Size != 4096 {
			b.Fatalf("scan results: %+v", items)
		}
	}
}

func BenchmarkLeftoverFrame(b *testing.B) {
	m := newModel(false)
	m.target, m.phase = &App{Name: "Example"}, phaseResults
	for i := range 2000 {
		m.leftovers = append(m.leftovers, Leftover{Kind: KindKey, Hive: registry.CURRENT_USER,
			Path: fmt.Sprintf(`Software\Example\Settings%04d`, i), Reason: "Settings key named after the app", Sure: true, Selected: true})
	}
	tt := ui.NewTester(m.view, 1060, 700)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tt.Frame()
	}
}

func BenchmarkLeftoverScale(b *testing.B) {
	for _, n := range []int{100, 5000, 50000} {
		for _, expanded := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/expanded=%v", n, expanded), func(b *testing.B) {
				m := largeLeftoverModel(n, true)
				tt := ui.NewTester(m.view, 1060, 700)
				tt.Frame()
				if expanded {
					m.foldAll(false)
					tt.Frame()
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					tt.Frame()
				}
			})
		}
	}
}

func BenchmarkLeftoverSearch(b *testing.B) {
	for _, n := range []int{5000, 50000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			m := largeLeftoverModel(n, false)
			tt := ui.NewTester(m.view, 1060, 700)
			tt.Frame()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if m.leftoverQuery == "entry-" {
					m.leftoverQuery = "entry-04999"
				} else {
					m.leftoverQuery = "entry-"
				}
				tt.Frame()
			}
		})
	}
}
