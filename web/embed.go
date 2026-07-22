// Package web exposes the production dashboard assets embedded in the Go
// binary. The committed dist tree keeps ordinary Go builds reproducible.
package web

import "embed"

// FS contains the Vite production build under dist/.
//
//go:embed dist/*
var FS embed.FS
