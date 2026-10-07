// Package web embute a UI do painel no binário — o telão não precisa de
// internet nem de CDN — e a instrução para assistentes de IA (ia/).
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// FS é a raiz servida em "/".
var FS, _ = fs.Sub(static, "static")

// IA contém o template da instrução de integração para assistentes de IA e
// os clientes de referência (Python/TS) que ela embute.
//
//go:embed ia
var IA embed.FS
