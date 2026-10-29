// Package ui is the desktop window: a local page and the API behind it.
package ui

import "embed"

// Assets holds the window's HTML, CSS, script, and logo.
//
//go:embed assets/*
var Assets embed.FS

// LogoPNG is the app icon, used by the Mac window and the page.
func LogoPNG() ([]byte, error) {
	return Assets.ReadFile("assets/logo.png")
}
