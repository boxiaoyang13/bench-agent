// Package web 打包前端静态资源（index.html / app.js / styles.css）。
//
// 页面是无构建的原生 SPA，使用 hash 路由，服务端无需 SPA fallback。
package web

import "embed"

//go:embed index.html app.js styles.css
var Assets embed.FS
