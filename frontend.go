package main

import (
	"embed"
	"io/fs"

	"github.com/egoist/mygo"
)

// Embed the built frontend so plain `go run .`/`go build` also serve it.
// `mygo build` embeds the same files through its generated init; whichever
// init runs last wins, with identical contents either way. `mygo dev`
// bypasses this entirely by pointing windows at the Vite dev server.
//
//go:embed all:dist
var frontendDist embed.FS

func init() {
	files, _ := fs.Sub(frontendDist, "dist")
	mygo.SetFrontend(files)
}
