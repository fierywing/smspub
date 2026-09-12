// Package smspub 把 web/ 前端三件套嵌入二进制（go:embed 不允许 ../ 路径，故放模块根）。
package smspub

import "embed"

//go:embed web
var WebFS embed.FS
