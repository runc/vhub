// VHub is a Douyin video/gallery downloader with a MyGo UI.
//
//	go run .          # after `npm run build:web` (or via `mygo dev`)
package main

import (
	"log"

	"github.com/egoist/mygo"
)

func main() {
	app := mygo.App
	app.SetName("VHub")

	settings, err := LoadSettings(nil)
	if err != nil {
		log.Fatal(err)
	}
	hub := NewHub(settings)
	mygo.Bind(hub, settings)

	app.WhenReady(func() {
		app.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
			{Role: mygo.RoleAppMenu},
			{Role: mygo.RoleEditMenu}, // Cmd+V paste in the URL field
			{Role: mygo.RoleViewMenu},
			{Role: mygo.RoleWindowMenu},
		}))
		openMainWindow()
	})
	app.OnWindowAllClosed(func() {}) // keep the app in the Dock
	app.OnActivate(func(hasVisibleWindows bool) {
		if !hasVisibleWindows {
			openMainWindow()
		}
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

func openMainWindow() {
	mygo.NewWindow(mygo.WindowOptions{
		Title:           "VHub",
		Width:           1260,
		Height:          820,
		MinWidth:        980,
		MinHeight:       620,
		TitleBarStyle:   mygo.TitleBarHidden,
		TitleBarHeight:  44,
		BackgroundColor: "light-dark(#eef0f4, #0b0d12)",
		URL:             "/",
	})
}
