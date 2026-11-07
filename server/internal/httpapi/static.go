package httpapi

import (
	"io/fs"
	"net/http"
	"os"

	"github.com/apexcloud/thvv/server/web"
)

// uiHandler 提供 Web 页面：优先磁盘目录（改页面免重编译），否则用二进制内置版本。
//
// 静态资源一律加 Cache-Control: no-cache：允许缓存但每次必须回源校验
// （Go FileServer 支持 If-Modified-Since → 304，代价可忽略）。
// 不能不设：浏览器会对无 Cache-Control 的 200 做启发式缓存，
// 出现「index.html 已更新、app.js 还是旧缓存」的白屏（UI 刷不出来）。
func (s *Server) uiHandler() http.Handler {
	uiPath := trimSlash(orDefault(s.cfg.Server.UI.Path, "/ui/"))

	var h http.Handler
	if dir := string(s.cfg.Server.UI.Dir); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			h = http.StripPrefix(uiPath, http.FileServerFS(os.DirFS(dir)))
		} else {
			s.log.Warn("server.ui.dir 不可用，改用内置页面", "dir", dir)
		}
	}
	if h == nil {
		sub, err := fs.Sub(web.Assets, ".")
		if err != nil {
			return http.NotFoundHandler()
		}
		h = http.StripPrefix(uiPath, http.FileServerFS(sub))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func trimSlash(s string) string {
	for len(s) > 1 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
