package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"
)

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func atofDefault(s string, def float64) float64 {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeout(r.Context(), d)
}

func statDir(p string) (os.FileInfo, error) {
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		if err == nil {
			err = os.ErrInvalid
		}
		return nil, err
	}
	return st, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func sortStrings(v []string) { sort.Strings(v) }

// isPrivateHost 判断 URL 是否指向内网/回环（SSRF 防护）。
func isPrivateHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	// 域名：解析一次（失败则放行，交给连通性检查报错）
	ips, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}
