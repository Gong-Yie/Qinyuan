package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// 控制台静态资源：手写 SPA，构建期由 go:embed 打进二进制。
//
// 为什么不是 Next.js 静态导出（实现规格/06 的原方案）：这个仓库没有前端构建链，
// 而 M5 的目标是"能看见它在干什么"。手写 SPA 满足同一条架构要求
// （静态文件 + 单二进制 + 同端口），代价是没有组件生态 —— 记在 docs/11。
//
//go:embed webui
var webuiFS embed.FS

// staticHandler 提供控制台页面与静态资源。
//
// 规则：
//   - /api/* 打到这里说明接口不存在（路由表没匹配上），返回 404 而不是 index.html；
//   - index.html 不缓存（改完刷新就能看到），资源短缓存；
//   - 其它路径交给 FileServer（它会自己处理目录/404）。
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "接口不存在")
			return
		}
		// 控制台还在迭代：一律 no-cache（每次都回源校验），避免"改了样式刷新没变"。
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
