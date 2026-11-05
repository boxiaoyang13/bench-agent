package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

// 会话 token 结构：base64(payload) + "." + base64(hmac(secret, payload))
// payload = username \x00 role \x00 expUnix
func (s *Server) signSession(username, role string, exp time.Time) string {
	payload := username + "\x00" + role + "\x00" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.cfg.Auth.Session.SigningKey))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) parseSession(v string) (username, role string, ok bool) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.Auth.Session.SigningKey))
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", "", false
	}
	f := strings.Split(string(raw), "\x00")
	if len(f) != 3 {
		return "", "", false
	}
	exp, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", "", false
	}
	return f[0], f[1], true
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 校验用户名口令并下发会话 Cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !s.decode(w, r, &req) {
		return
	}
	var target *struct {
		hash string
		role string
	}
	for i := range s.cfg.Auth.Users {
		u := &s.cfg.Auth.Users[i]
		if u.Username == req.Username {
			target = &struct{ hash, role string }{u.PasswordHash, u.Role}
			break
		}
	}
	if target == nil || bcrypt.CompareHashAndPassword([]byte(target.hash), []byte(req.Password)) != nil {
		// 统一错误，不区分用户是否存在
		writeProblem(w, r, domain.NewProblem(401, "unauthorized", "用户名或密码错误"))
		return
	}

	exp := time.Now().Add(s.cfg.Auth.Session.TTL)
	val := s.signSession(req.Username, target.role, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.Auth.Session.CookieName,
		Value:    val,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(s.cfg.Auth.Session.TTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: sameSite(s.cfg.Auth.Session.SameSite),
	})
	s.st.Audit(r.Context(), req.Username, "auth.login", "session", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout 清除会话。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.Auth.Session.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.Auth.Session.Secure,
		SameSite: sameSite(s.cfg.Auth.Session.SameSite),
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleSession 返回当前登录主体。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if p == nil {
		writeProblem(w, r, domain.NewProblem(401, "unauthorized", "未登录"))
		return
	}
	var exp *time.Time
	if c, err := r.Cookie(s.cfg.Auth.Session.CookieName); err == nil {
		if e, ok := s.sessionExpiry(c.Value); ok {
			exp = &e
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username":   p.Name,
		"roles":      []string{p.Role},
		"kind":       p.Kind,
		"expires_at": exp,
	})
}

func (s *Server) sessionExpiry(v string) (time.Time, bool) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return time.Time{}, false
	}
	f := strings.Split(string(raw), "\x00")
	if len(f) != 3 {
		return time.Time{}, false
	}
	exp, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(exp, 0).UTC(), true
}

func sameSite(v string) http.SameSite {
	switch strings.ToLower(v) {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}
