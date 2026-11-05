package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Issue 是一条校验结果。
type Issue struct {
	Level string // error | warn
	Field string
	Msg   string
}

func (i Issue) String() string {
	prefix := "✕"
	if i.Level == "warn" {
		prefix = "!"
	}
	if i.Field == "" {
		return fmt.Sprintf("  %s %s", prefix, i.Msg)
	}
	return fmt.Sprintf("  %s %s: %s", prefix, i.Field, i.Msg)
}

// Validate 做语义校验，fail-fast（有任何 error 就返回）。
func (c *Config) Validate() error {
	issues := c.Check()
	var errs []string
	for _, is := range issues {
		if is.Level == "error" {
			errs = append(errs, is.String())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("配置校验失败:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}

// Check 返回全部问题（含 warn），供 -check 输出。
func (c *Config) Check() []Issue {
	var out []Issue
	add := func(level, field, msg string) { out = append(out, Issue{level, field, msg}) }
	errf := func(field, format string, a ...any) { add("error", field, fmt.Sprintf(format, a...)) }
	warnf := func(field, format string, a ...any) { add("warn", field, fmt.Sprintf(format, a...)) }

	// 0. version
	if c.Version != 1 {
		errf("version", "不支持的版本 %d（仅支持 1）", c.Version)
	}

	// 1. roots
	if c.Roots.Thvv == "" && c.needsTHVV() {
		errf("roots.thvv", "必填（或用 -thvv-home / THVV_ROOTS__THVV 提供）")
	}
	if v, ok := c.Resolved["roots.thvv"]; ok && c.needsTHVV() {
		if st, err := os.Stat(v); err != nil || !st.IsDir() {
			errf("roots.thvv", "%s 不存在或不是目录", v)
		} else {
			// 指纹：确认这是 THVV 根
			if !exists(filepath.Join(v, "perf", "run.sh")) && !exists(filepath.Join(v, "quickstart.sh")) {
				errf("roots.thvv", "%s 看起来不是 THVV 根目录（未找到 perf/run.sh 或 quickstart.sh）", v)
			}
			if !exists(filepath.Join(v, "perf")) {
				errf("roots.thvv", "%s 缺少 perf/ 目录", v)
			}
		}
	}
	if v, ok := c.Resolved["roots.data"]; ok {
		if err := os.MkdirAll(v, 0o755); err != nil {
			errf("roots.data", "%s 不可写: %v", v, err)
		} else if err := probeWritable(v); err != nil {
			errf("roots.data", "%s 不可写: %v", v, err)
		}
	}

	// 2. secrets
	if len(c.Secrets.MasterKey) != 32 {
		errf("secrets", "主密钥长度必须为 32 字节（当前 %d）", len(c.Secrets.MasterKey))
	}

	// 3. auth
	switch c.Auth.Mode {
	case "bearer", "cookie", "both":
	default:
		errf("auth.mode", "必须是 bearer | cookie | both（当前 %q）", c.Auth.Mode)
	}
	if (c.Auth.Mode == "bearer" || c.Auth.Mode == "both") && len(c.Auth.Tokens) == 0 {
		errf("auth.tokens", "mode=%s 时至少需要一个 API Token", c.Auth.Mode)
	}
	if (c.Auth.Mode == "cookie" || c.Auth.Mode == "both") && len(c.Auth.Users) == 0 {
		errf("auth.users", "mode=%s 时至少需要一个用户", c.Auth.Mode)
	}
	names := map[string]bool{}
	for i, t := range c.Auth.Tokens {
		if t.Token == "" || strings.HasPrefix(t.Token, "${") {
			errf(fmt.Sprintf("auth.tokens[%d].token", i), "未设置")
		}
		if !validRole(t.Role) {
			errf(fmt.Sprintf("auth.tokens[%d].role", i), "必须是 viewer | operator | admin")
		}
		if names[t.Name] {
			errf(fmt.Sprintf("auth.tokens[%d].name", i), "重复: %s", t.Name)
		}
		names[t.Name] = true
	}
	for i, u := range c.Auth.Users {
		if u.Username == "" {
			errf(fmt.Sprintf("auth.users[%d].username", i), "必填")
		}
		if !isBcrypt(u.PasswordHash) {
			errf(fmt.Sprintf("auth.users[%d].password_hash", i), "不是合法的 bcrypt hash（$2a$/$2b$/$2y$ 开头）")
		}
		if !validRole(u.Role) {
			errf(fmt.Sprintf("auth.users[%d].role", i), "必须是 viewer | operator | admin")
		}
	}

	// 4. store
	if c.Store.Driver != "sqlite" {
		errf("store.driver", "v1 仅支持 sqlite（当前 %q）", c.Store.Driver)
	}

	// 5. server
	if c.Server.WriteTimeout != 0 && c.anySubprocessEngine() {
		warnf("server.write_timeout", "非 0 会掐断 SSE 进度流，建议设为 0s")
	}
	if c.Server.TLS.Enabled {
		if !exists(c.Server.TLS.CertFile) {
			errf("server.tls.cert_file", "%s 不存在", c.Server.TLS.CertFile)
		}
		if !exists(c.Server.TLS.KeyFile) {
			errf("server.tls.key_file", "%s 不存在", c.Server.TLS.KeyFile)
		}
	}
	if c.Server.UI.Enabled && c.Server.UI.Dir != "" && !exists(c.Server.UI.Dir) {
		warnf("server.ui.dir", "%s 不存在，将使用二进制内置页面", c.Server.UI.Dir)
	}

	// 6. gates
	if c.Gates.DefaultProfile != "" {
		if _, ok := c.Gates.Profiles[c.Gates.DefaultProfile]; !ok {
			errf("gates.default_profile", "未知的模板 %q", c.Gates.DefaultProfile)
		}
	}
	for name, p := range c.Gates.Profiles {
		if p == nil {
			continue
		}
		prevHi := -1
		for i, t := range p.Tiers {
			if t.Lo >= t.Hi {
				errf(fmt.Sprintf("gates.%s.tiers[%d]", name, i), "lo(%d) 必须小于 hi(%d)", t.Lo, t.Hi)
			}
			if prevHi >= 0 && t.Lo < prevHi {
				errf(fmt.Sprintf("gates.%s.tiers[%d]", name, i), "与上一档区间重叠")
			}
			prevHi = t.Hi
		}
	}

	// 7. reports
	if len(c.Reports.DefaultFormats) == 0 {
		errf("reports.default_formats", "至少一种格式")
	}
	for _, f := range c.Reports.DefaultFormats {
		if f != "html" && f != "xlsx" && f != "csv" && f != "json" {
			errf("reports.default_formats", "不支持的格式 %q", f)
		}
	}
	for field, s := range map[string]string{
		"reports.title.single": c.Reports.Title.Single,
		"reports.title.matrix": c.Reports.Title.Matrix,
	} {
		for _, v := range tplVars(s) {
			if !allowedTplVar[v] {
				errf(field, "未知模板变量 {%s}（可用: model bucket ts vendor company）", v)
			}
		}
	}
	if n := len([]rune(c.Reports.XLSX.SheetMetrics)); n > 31 {
		errf("reports.xlsx.sheet_metrics", "sheet 名超过 31 字符（%d）", n)
	}
	if strings.ContainsAny(c.Reports.XLSX.SheetMetrics+c.Reports.XLSX.SheetFailures, `[]:*?/\`) {
		errf("reports.xlsx", "sheet 名不能包含 []:*?/\\ 之一")
	}

	// 8. engines
	if len(c.Engines) == 0 {
		errf("engines", "至少配置一个引擎")
	}
	pythonSeen := map[string]bool{}
	for name, eng := range c.Engines {
		if eng == nil {
			continue
		}
		switch eng.Kind {
		case "subprocess", "builtin", "http":
		default:
			errf("engines."+name+".kind", "必须是 subprocess | builtin | http（当前 %q）", eng.Kind)
		}
		if !eng.Enabled {
			continue
		}
		if eng.Kind == "http" && eng.BaseURL == "" {
			errf("engines."+name+".base_url", "kind=http 时必填")
		}
		if len(eng.Capabilities) == 0 {
			errf("engines."+name+".capabilities", "至少配置一个 capability")
		}
		for capName, cap := range eng.Capabilities {
			if cap == nil || !cap.Enabled {
				continue
			}
			base := fmt.Sprintf("engines.%s.%s", name, capName)
			if eng.Kind == "subprocess" {
				rt := cap.Runtime
				if rt == nil {
					rt = eng.Runtime
				}
				if rt == nil {
					errf(base+".runtime", "kind=subprocess 时必须配置 runtime（或引擎级 runtime）")
				} else {
					py := string(rt.Python)
					if py == "" {
						errf(base+".runtime.python", "必填")
					} else if !pythonSeen[py] {
						pythonSeen[py] = true
						if !isExecutable(py) {
							errf(base+".runtime.python", "%s 不可执行", py)
						}
					}
					if capName == "perf" || capName == "eval" {
						if string(rt.Evalscope) == "" {
							errf(base+".runtime.evalscope", "必填")
						} else if !isExecutable(string(rt.Evalscope)) {
							errf(base+".runtime.evalscope", "%s 不可执行", rt.Evalscope)
						}
					}
				}
				if capName == "perf" && cap.WorkingDir == "" {
					errf(base+".working_dir", "必填")
				}
				if capName == "perf" && cap.WorkingDir != "" && !exists(string(cap.WorkingDir)) {
					errf(base+".working_dir", "%s 不存在", cap.WorkingDir)
				}
				if capName == "eval" {
					if cap.Script == "" {
						errf(base+".script", "必填")
					} else if !exists(string(cap.Script)) {
						errf(base+".script", "%s 不存在", cap.Script)
					}
				}
			}
			if capName == "perf" {
				checkPerf(name, cap, errf, warnf)
			}
		}
	}

	// 9. datasets
	//   - 需要 fork Python 时必须真实存在（引擎要按 token 区间过滤样本）
	//   - builtin 模拟引擎不读数据集，缺了只 warn，便于「无需 Python」的本地联调
	if c.perfEnabled() {
		if v, ok := c.Resolved["paths.datasets_dir"]; ok {
			if !exists(v) {
				fallback := filepath.Join(filepath.Dir(v), "datasets")
				switch {
				case exists(fallback):
					warnf("paths.datasets_dir", "%s 不存在，回落到 %s", v, fallback)
					c.Paths.DatasetsDir = Path(fallback)
				case c.needsTHVV():
					errf("paths.datasets_dir", "%s 与回落目录均不存在", v)
				default:
					warnf("paths.datasets_dir", "%s 不存在（builtin 模拟引擎不读数据集，可忽略）", v)
				}
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Level < out[j].Level })
	return out
}

func checkPerf(engine string, cap *Capability, errf, warnf func(string, string, ...any)) {
	base := "engines." + engine + ".perf"
	if len(cap.Buckets) == 0 {
		errf(base+".buckets", "至少一个档位")
	}
	seen := map[string]bool{}
	for i, b := range cap.Buckets {
		f := fmt.Sprintf("%s.buckets[%d]", base, i)
		if b.Label == "" {
			errf(f+".label", "必填")
		}
		if seen[b.Label] {
			errf(f+".label", "重复: %s", b.Label)
		}
		seen[b.Label] = true
		if b.Min >= b.Max {
			errf(f, "min(%d) 必须小于 max(%d)", b.Min, b.Max)
		}
		if b.MaxParallel <= 0 {
			errf(f+".max_parallel", "必须大于 0")
		}
	}
	for p, n := range cap.NumberByParallel {
		if n <= 0 {
			errf(fmt.Sprintf("%s.number_by_parallel[%d]", base, p), "请求数必须大于 0")
		}
	}
	for i, r := range cap.Defaults.TokenizerMap {
		if _, err := reCompile(r.Match); err != nil {
			errf(fmt.Sprintf("%s.defaults.tokenizer_map[%d].match", base, i), "正则非法: %v", err)
		}
	}
	for i, r := range cap.Defaults.TemperatureMap {
		if _, err := reCompile(r.Match); err != nil {
			errf(fmt.Sprintf("%s.defaults.temperature_map[%d].match", base, i), "正则非法: %v", err)
		}
	}
	if len(cap.Defaults.TemperatureMap) > 0 {
		last := cap.Defaults.TemperatureMap[len(cap.Defaults.TemperatureMap)-1]
		if last.Match != "(?i).*" && last.Match != ".*" {
			warnf(base+".defaults.temperature_map", "最后一条不是兜底规则 .*，未命中模型将报错")
		}
	}
	if cap.Matrix != nil && len(cap.Matrix.Buckets) > 0 {
		for _, b := range cap.Matrix.Buckets {
			if !seen[b] {
				errf(base+".matrix.buckets", "引用了未定义档位 %q", b)
			}
		}
	}
}

// needsTHVV 判断是否真的需要 THVV Python 工具链（只有 fork evalscope 才需要）。
// builtin（内置模拟引擎）与 http（外部工具）都不需要。
func (c *Config) needsTHVV() bool {
	for _, e := range c.Engines {
		if e == nil || !e.Enabled || e.Kind != "subprocess" {
			continue
		}
		for _, cap := range e.Capabilities {
			if cap != nil && cap.Enabled {
				return true
			}
		}
	}
	return false
}

func (c *Config) perfEnabled() bool {
	for _, e := range c.Engines {
		if e == nil || !e.Enabled {
			continue
		}
		if p := e.Capabilities["perf"]; p != nil && p.Enabled {
			return true
		}
	}
	return false
}

func (c *Config) anySubprocessEngine() bool {
	for _, e := range c.Engines {
		if e != nil && e.Enabled && e.Kind == "subprocess" {
			return true
		}
	}
	return false
}

// ---------- 小工具 ----------

var allowedTplVar = map[string]bool{"model": true, "bucket": true, "ts": true, "vendor": true, "company": true}

func tplVars(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+j])
		s = s[i+j+1:]
	}
}

func validRole(r string) bool { return r == "viewer" || r == "operator" || r == "admin" }

func isBcrypt(h string) bool {
	return strings.HasPrefix(h, "$2a$") || strings.HasPrefix(h, "$2b$") || strings.HasPrefix(h, "$2y$")
}

func exists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Mode()&0o111 != 0
}

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".thvv-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
