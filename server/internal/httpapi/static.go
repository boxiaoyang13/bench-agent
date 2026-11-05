package httpapi

import (
	"io/fs"
	"net/http"
	"os"

	"github.com/apexcloud/thvv/server/web"
)

// uiHandler 提供 Web 页面：优先磁盘目录（改页面免重编译），否则用二进制内置版本。
func (s *Server) uiHandler() http.Handler {
	uiPath := trimSlash(orDefault(s.cfg.Server.UI.Path, "/ui/"))

	if dir := string(s.cfg.Server.UI.Dir); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return http.StripPrefix(uiPath, http.FileServerFS(os.DirFS(dir)))
		}
		s.log.Warn("server.ui.dir 不可用，改用内置页面", "dir", dir)
	}
	sub, err := fs.Sub(web.Assets, ".")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix(uiPath, http.FileServerFS(sub))
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
