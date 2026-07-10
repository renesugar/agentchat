// Command agentchat is the Wails desktop app: a chat GUI whose "models"
// are terminal coding agents (Claude Code, Codex, Aider, Swival). See
// ../ARCHITECTURE.md for the design and README.md here for build steps.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app, err := NewApp("")
	if err != nil {
		log.Fatal(err)
	}

	if err := wails.Run(&options.App{
		Title:            "AgentChat",
		Width:            1280,
		Height:           840,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: &options.RGBA{R: 0x10, G: 0x14, B: 0x18, A: 0xFF},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
	}); err != nil {
		log.Fatal(err)
	}
}
