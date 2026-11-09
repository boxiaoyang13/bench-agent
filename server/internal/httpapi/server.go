// Package httpapi 实现 HTTP 协议（见 docs/API.md 与 docs/openapi.yaml）。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/runmgr"
	"github.com/apexcloud/thvv/server/internal/secret"
	"github.com/apexcloud/thvv/server/internal/store"
)

// Server 是 HTTP 服务。
type Server struct {
	cfg     *config.Config
	st      *store.Store
	reg     *engine.Registry
	mgr     *runmgr.Manager
	box     *secret.Box
	log     *slog.Logger
	started time.Time
	version string
	commit  string
}

// New 构造 HTTP 服务。
func New(cfg *config.Config, st *store.Store, reg *engine.Registry, mgr *runmgr.Manager,
	box *secret.Box, log *slog.Logger, version, commit string) *Server {
	return &Server{cfg: cfg, st: st, reg: reg, mgr: mgr, box: box, log: log,
		started: time.Now(), version: version, commit: commit}
}

// Handler 组装路由与中间件。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	bp := s.cfg.Server.BasePath // 例如 /v1

	// ---- system ----
	mux.HandleFunc("GET "+bp+"/health", s.handleHealth)
	mux.HandleFunc("GET "+bp+"/meta", s.handleMeta)
	mux.HandleFunc("GET "+bp+"/tools", s.handleTools)
	mux.HandleFunc("GET "+bp+"/catalog/buckets", s.handleCatalogBuckets)
	mux.HandleFunc("GET "+bp+"/catalog/datasets", s.handleCatalogDatasets)
	mux.HandleFunc("GET "+bp+"/catalog/gates", s.handleCatalogGates)

	// ---- auth (Web) ----
	mux.HandleFunc("POST "+bp+"/auth/login", s.handleLogin)
	mux.HandleFunc("POST "+bp+"/auth/logout", s.handleLogout)
	mux.HandleFunc("GET "+bp+"/auth/session", s.handleSession)

	// ---- targets ----
	mux.HandleFunc("POST "+bp+"/targets", s.handleCreateTarget)
	mux.HandleFunc("GET "+bp+"/targets", s.handleListTargets)
	mux.HandleFunc("GET "+bp+"/targets/{target_id}", s.handleGetTarget)
	mux.HandleFunc("PATCH "+bp+"/targets/{target_id}", s.handlePatchTarget)
	mux.HandleFunc("DELETE "+bp+"/targets/{target_id}", s.handleDeleteTarget)
	mux.HandleFunc("POST "+bp+"/targets/{target_id}/probe", s.handleProbeTarget)

	// ---- runs ----
	mux.HandleFunc("POST "+bp+"/runs", s.handleCreateRun)
	mux.HandleFunc("GET "+bp+"/runs", s.handleListRuns)
	mux.HandleFunc("GET "+bp+"/runs/{run_id}", s.handleGetRun)
	mux.HandleFunc("POST "+bp+"/runs/{run_id}/cancel", s.handleCancelRun)
	mux.HandleFunc("DELETE "+bp+"/runs/{run_id}", s.handleDeleteRun)
	mux.HandleFunc("POST "+bp+"/runs/{run_id}/rerun", s.handleRerunRun)
	mux.HandleFunc("POST "+bp+"/runs/purge", s.handlePurgeRuns)
	mux.HandleFunc("GET "+bp+"/runs/{run_id}/events", s.handleRunEvents)
	mux.HandleFunc("GET "+bp+"/runs/{run_id}/results", s.handleRunResults)
	mux.HandleFunc("GET "+bp+"/runs/{run_id}/results/failures", s.handleRunFailures)
	mux.HandleFunc("GET "+bp+"/runs/{run_id}/artifacts", s.handleRunArtifacts)
	mux.HandleFunc("POST "+bp+"/runs/{run_id}/reports", s.handleRegenerateReports)

	// ---- artifacts ----
	mux.HandleFunc("GET "+bp+"/artifacts/{artifact_id}", s.handleDownloadArtifact)

	// ---- UI ----
	if s.cfg.Server.UI.Enabled {
		uiPath := trimSlash(orDefault(s.cfg.Server.UI.Path, "/ui/"))
		mux.Handle("GET "+uiPath+"/", s.uiHandler())
		if s.cfg.Server.UI.RedirectRoot {
			mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					http.NotFound(w, r)
					return
				}
				http.Redirect(w, r, uiPath+"/", http.StatusFound)
			})
		}
	}
	if s.cfg.Observability.Metrics.Enabled {
		mux.HandleFunc("GET "+orDefault(s.cfg.Observability.Metrics.Path, "/metrics"), s.handleMetrics)
	}

	return s.recoverMW(s.accessLogMW(s.corsMW(s.authMW(mux))))
}

// ---------------- 中间件 ----------------

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxRequestID
)

type principal struct {
	Name string
	Role string
	Kind string // token | cookie
}

func (p *principal) canWrite() bool { return p != nil && (p.Role == "operator" || p.Role == "admin") }
func (p *principal) isAdmin() bool  { return p != nil && p.Role == "admin" }

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeProblem(w, r, domain.NewProblem(500, "internal_error", "服务内部错误"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLogMW(next http.Handler) http.Handler {
	if !s.cfg.Observability.AccessLog {
		return next
	}
	slow := s.cfg.Observability.SlowRequestThreshold
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)
		// SSE 与静态资源不打印，避免噪音
		if strings.HasSuffix(r.URL.Path, "/events") && rw.status == 200 {
			return
		}
		lvl := slog.LevelInfo
		if dur > slow {
			lvl = slog.LevelWarn
		}
		s.log.Log(r.Context(), lvl, "http",
			"method", r.Method, "path", r.URL.Path, "status", rw.status,
			"dur_ms", dur.Milliseconds(), "ip", clientIP(r))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) corsMW(next http.Handler) http.Handler {
	if !s.cfg.Server.CORS.Enabled {
		return next
	}
	allowed := map[string]bool{}
	for _, o := range s.cfg.Server.CORS.AllowedOrigins {
		allowed[o] = true
	}
	hdrs := strings.Join(orSlice(s.cfg.Server.CORS.AllowedHeaders,
		[]string{"Authorization", "Content-Type", "Idempotency-Key", "Last-Event-ID"}), ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowed["*"] || allowed[origin]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", hdrs)
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(int(s.cfg.Server.CORS.MaxAge.Seconds())))
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authMW 先查 Cookie 会话，再查 Bearer token；写操作需要 operator 及以上。
func (s *Server) authMW(next http.Handler) http.Handler {
	bp := s.cfg.Server.BasePath
	public := map[string]bool{
		bp + "/health":      true,
		bp + "/auth/login":  true,
		bp + "/auth/logout": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if public[path] || (!strings.HasPrefix(path, bp) && !strings.HasPrefix(path, "/ui")) {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(path, bp) {
			next.ServeHTTP(w, r) // 静态资源
			return
		}
		if path == bp+"/auth/session" {
			if p := s.identify(r); p != nil {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
				return
			}
			writeProblem(w, r, domain.NewProblem(401, "unauthorized", "未登录或会话已过期"))
			return
		}

		p := s.identify(r)
		if p == nil {
			writeProblem(w, r, domain.NewProblem(401, "unauthorized", "缺少或无效的凭据"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !p.canWrite() {
			writeProblem(w, r, domain.NewProblem(403, "forbidden", "当前角色无权执行写操作"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	})
}

func (s *Server) identify(r *http.Request) *principal {
	// 1) Cookie 会话
	if c, err := r.Cookie(s.cfg.Auth.Session.CookieName); err == nil {
		if name, role, ok := s.parseSession(c.Value); ok {
			return &principal{Name: name, Role: role, Kind: "cookie"}
		}
	}
	// 2) Bearer token
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		for _, t := range s.cfg.Auth.Tokens {
			if subtleEqual(t.Token, tok) {
				return &principal{Name: t.Name, Role: t.Role, Kind: "token"}
			}
		}
	}
	return nil
}

func principalOf(r *http.Request) *principal {
	if p, ok := r.Context().Value(ctxPrincipal).(*principal); ok {
		return p
	}
	return nil
}

// ---------------- 通用工具 ----------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeProblem 输出 RFC 9457 problem+json。
func writeProblem(w http.ResponseWriter, r *http.Request, p *domain.Problem) {
	if p.Status == 0 {
		p.Status = 500
	}
	if p.Type == "" {
		p.Type = "https://thvv.dev/errors/" + p.Code
	}
	if p.Title == "" {
		p.Title = strings.ReplaceAll(p.Code, "_", " ")
	}
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	if p.Retryable {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "5")
		}
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, s.cfg.Limits.API.MaxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeProblem(w, r, domain.NewProblem(400, "invalid_request", "请求体为空"))
			return false
		}
		writeProblem(w, r, domain.NewProblem(400, "invalid_request", "JSON 解析失败: "+err.Error()))
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func orSlice(v, def []string) []string {
	if len(v) == 0 {
		return def
	}
	return v
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func itoa(i int) string { return strconv.Itoa(i) }

func errProblem(err error) *domain.Problem {
	var ve *runmgr.ValidationError
	if errors.As(err, &ve) {
		return domain.NewProblem(400, "validation_failed", ve.Msg)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return domain.NewProblem(404, "not_found", "资源不存在")
	case errors.Is(err, store.ErrConflict):
		return domain.NewProblem(409, "conflict", "资源冲突（名称重复或幂等键参数不一致）")
	case errors.Is(err, runmgr.ErrQueueFull):
		p := domain.NewProblem(429, "quota_exceeded", "服务端并发/队列已满，请稍后重试")
		p.Retryable = true
		return p
	case errors.Is(err, runmgr.ErrEphemeralRerun):
		return domain.NewProblem(409, "conflict", err.Error())
	}
	return domain.NewProblem(500, "internal_error", fmt.Sprintf("%v", err))
}
