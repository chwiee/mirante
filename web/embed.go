// Package web embute a UI do painel no binário — o telão não precisa de
// internet nem de CDN.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// FS é a raiz servida em "/".
var FS, _ = fs.Sub(static, "static")
