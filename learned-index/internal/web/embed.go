package web

import "embed"

// siteDir 是嵌入前端资源的 http.FileSystem 根。
//
//go:embed site
var siteFS embed.FS
