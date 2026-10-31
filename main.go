// Traceless removes Windows apps and what they leave behind: it runs an
// app's own uninstaller, then offers to find the folders, files, shortcuts
// and registry entries that are left, and removes those the user picks, the
// files to the Recycle Bin and the registry backed up first.
package main

import (
	"log"
	"os"
	"slices"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func main() {
	// Removing apps installed for all users, and their leftovers, takes
	// administrator rights: ask for them once, as the app starts. When the
	// user declines, the app runs with what it has.
	elevated := isElevated()
	if !elevated && !slices.Contains(os.Args[1:], "--elevated") && os.Getenv("TRACELESS_NO_ELEVATE") == "" {
		if exe, err := os.Executable(); err == nil && relaunchElevated(exe) == nil {
			return
		}
	}

	m := newModel(elevated)
	mygo.App.WhenReady(func() {
		m.win = mygo.NewWindow(mygo.WindowOptions{
			Title:     "Traceless",
			Width:     1060,
			Height:    700,
			MinWidth:  720,
			MinHeight: 480,
			StateKey:  "main",
			// The view draws the title bar, over Windows 11's Mica, as the
			// system's own apps do.
			TitleBarStyle: mygo.TitleBarHidden,
			Vibrancy:      mygo.VibrancyMica,
			Content:       ui.View(m.view),
		})
		m.start()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
