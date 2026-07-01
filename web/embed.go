package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var embeddedFiles embed.FS

// MustSubFS 返回嵌入的 dist 子目录
// 用于在服务端直接读取前端打包产物；若嵌入目录缺失则 panic。
func MustSubFS() fs.FS {
	f, err := fs.Sub(embeddedFiles, "dist")
	if err != nil {
		panic(err)
	}
	return f
}
