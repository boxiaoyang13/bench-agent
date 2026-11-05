package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Path 是用前缀语法书写、最终被解析成绝对路径的字段类型。
//
//	thvv:perf/scripts → <roots.thvv>/perf/scripts
//	data:runs         → <roots.data>/runs
//	bin:python        → <roots.bin>/python
//	cfg:templates     → <配置文件目录>/templates
//	/abs 或无前缀      → 绝对路径 / 相对配置文件目录
type Path string

func (p Path) String() string { return string(p) }

var prefixRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):(.*)$`)

// Resolver 负责根与路径的两阶段解析。
type Resolver struct {
	ConfigDir string
	roots     map[string]string // name → 绝对路径
}

// NewResolver 解析 roots（允许根引用根，按声明顺序，检测循环）。
func NewResolver(configDir string, roots Roots) (*Resolver, error) {
	r := &Resolver{ConfigDir: configDir, roots: map[string]string{}}
	r.roots["cfg"] = configDir

	decl := map[string]string{"thvv": roots.Thvv, "data": roots.Data, "bin": roots.Bin}
	order := []string{"thvv", "data", "bin"}
	extNames := make([]string, 0, len(roots.Ext))
	for k := range roots.Ext {
		extNames = append(extNames, k)
	}
	sort.Strings(extNames)
	order = append(order, extNames...)
	for _, k := range extNames {
		decl[k] = roots.Ext[k]
	}

	var resolve func(name string, stack []string) (string, error)
	resolve = func(name string, stack []string) (string, error) {
		for _, s := range stack {
			if s == name {
				return "", fmt.Errorf("roots: 检测到循环引用 %s", strings.Join(append(stack, name), " → "))
			}
		}
		if v, ok := r.roots[name]; ok {
			return v, nil
		}
		raw, ok := decl[name]
		if !ok || raw == "" {
			return "", fmt.Errorf("roots.%s: 未配置", name)
		}
		// 根的值允许是 <other_root>:rel
		if m := prefixRe.FindStringSubmatch(strings.TrimSpace(raw)); m != nil && len(m[1]) > 1 {
			base, err := resolve(m[1], append(stack, name))
			if err != nil {
				return "", err
			}
			return cleanJoin(base, m[2]), nil
		}
		abs := expandHome(strings.TrimSpace(raw))
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(configDir, abs)
		}
		return filepath.Clean(abs), nil
	}

	for _, name := range order {
		if decl[name] == "" {
			continue
		}
		abs, err := resolve(name, nil)
		if err != nil {
			return nil, err
		}
		r.roots[name] = abs
	}
	return r, nil
}

// Roots 返回解析后的根表。
func (r *Resolver) Roots() map[string]string {
	out := make(map[string]string, len(r.roots))
	for k, v := range r.roots {
		out[k] = v
	}
	return out
}

// Resolve 把带前缀的路径解析成绝对路径；禁止 ../ 逃逸出根。
func (r *Resolver) Resolve(raw string, field string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s: 空路径", field)
	}
	m := prefixRe.FindStringSubmatch(raw)
	if m != nil && len(m[1]) > 1 {
		prefix, rel := m[1], m[2]
		base, ok := r.roots[prefix]
		if !ok {
			names := make([]string, 0, len(r.roots))
			for k := range r.roots {
				names = append(names, k)
			}
			sort.Strings(names)
			return "", fmt.Errorf("%s: 未知根 %q（可用: %s）", field, prefix, strings.Join(names, ", "))
		}
		abs := cleanJoin(base, rel)
		if !inside(abs, base) {
			return "", fmt.Errorf("%s: 路径逃逸出根 %q → %s", field, prefix, abs)
		}
		return abs, nil
	}
	abs := expandHome(raw)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.ConfigDir, abs)
	}
	return filepath.Clean(abs), nil
}

// ResolveEnvValue 用宽松规则解析 runtime.env 的值：
// 只有命中已知根前缀、或以 ~ / / 开头时才当路径，否则原样返回字面量。
func (r *Resolver) ResolveEnvValue(v string) string {
	m := prefixRe.FindStringSubmatch(v)
	if m != nil && len(m[1]) > 1 {
		if _, ok := r.roots[m[1]]; ok {
			if abs, err := r.Resolve(v, "env"); err == nil {
				return abs
			}
		}
		return v
	}
	if strings.HasPrefix(v, "~") || strings.HasPrefix(v, "/") {
		if abs, err := r.Resolve(v, "env"); err == nil {
			return abs
		}
	}
	return v
}

func cleanJoin(base, rel string) string {
	if rel == "" || rel == "." {
		return filepath.Clean(base)
	}
	return filepath.Clean(filepath.Join(base, rel))
}

func inside(target, base string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			if strings.HasPrefix(p, "~/") {
				return filepath.Join(home, p[2:])
			}
		}
	}
	return p
}

// resolvePaths 把配置里所有 Path 字段转成绝对路径，并记录到 Resolved。
func (c *Config) resolvePaths() error {
	r, err := NewResolver(c.ConfigDir, c.Roots)
	if err != nil {
		return err
	}
	c.Resolved = map[string]string{}
	for k, v := range r.Roots() {
		c.Resolved["roots."+k] = v
	}

	resolveOne := func(field string, p *Path) error {
		if strings.TrimSpace(string(*p)) == "" {
			return nil
		}
		abs, err := r.Resolve(string(*p), field)
		if err != nil {
			return err
		}
		*p = Path(abs)
		c.Resolved[field] = abs
		return nil
	}

	for _, it := range []struct {
		f string
		p *Path
	}{
		{"paths.runs_dir", &c.Paths.RunsDir},
		{"paths.logs_dir", &c.Paths.LogsDir},
		{"paths.datasets_dir", &c.Paths.DatasetsDir},
		{"paths.cache_dir", &c.Paths.CacheDir},
		{"paths.templates_dir", &c.Paths.TemplatesDir},
		{"store.sqlite.path", &c.Store.SQLite.Path},
		{"secrets.file.path", &c.Secrets.File.Path},
		{"server.ui.dir", (*Path)(&c.Server.UI.Dir)},
	} {
		if err := resolveOne(it.f, it.p); err != nil {
			return err
		}
	}

	for name, eng := range c.Engines {
		if eng == nil {
			continue
		}
		if eng.Runtime != nil {
			if err := c.resolveRuntime(r, "engines."+name+".runtime", eng.Runtime); err != nil {
				return err
			}
		}
		for capName, cap := range eng.Capabilities {
			if cap == nil {
				continue
			}
			base := "engines." + name + "." + capName
			if err := resolveOne(base+".working_dir", &cap.WorkingDir); err != nil {
				return err
			}
			if err := resolveOne(base+".script", &cap.Script); err != nil {
				return err
			}
			if cap.PlanSchema != "" {
				abs, err := r.Resolve(cap.PlanSchema, base+".plan_schema")
				if err != nil {
					return err
				}
				cap.PlanSchema = abs
				c.Resolved[base+".plan_schema"] = abs
			}
			if cap.Runtime != nil {
				if err := c.resolveRuntime(r, base+".runtime", cap.Runtime); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *Config) resolveRuntime(r *Resolver, field string, rt *Runtime) error {
	for _, it := range []struct {
		f string
		p *Path
	}{
		{field + ".home", &rt.Home},
		{field + ".python", &rt.Python},
		{field + ".evalscope", &rt.Evalscope},
	} {
		if strings.TrimSpace(string(*it.p)) == "" {
			continue
		}
		abs, err := r.Resolve(string(*it.p), it.f)
		if err != nil {
			return err
		}
		*it.p = Path(abs)
		c.Resolved[it.f] = abs
	}
	for i := range rt.PythonPathPrepend {
		abs, err := r.Resolve(string(rt.PythonPathPrepend[i]), fmt.Sprintf("%s.pythonpath_prepend[%d]", field, i))
		if err != nil {
			return err
		}
		rt.PythonPathPrepend[i] = Path(abs)
		c.Resolved[fmt.Sprintf("%s.pythonpath_prepend[%d]", field, i)] = abs
	}
	// env 值用宽松规则（不写进 Resolved 的路径表，仅就地展开）
	if len(rt.Env) > 0 {
		env := make(map[string]string, len(rt.Env))
		for k, v := range rt.Env {
			env[k] = r.ResolveEnvValue(v)
		}
		rt.Env = env
	}
	return nil
}

// resolveSecrets 取出 32 字节主密钥。
func (c *Config) resolveSecrets() error {
	switch strings.ToLower(strings.TrimSpace(c.Secrets.Provider)) {
	case "", "env":
		c.Secrets.Provider = "env"
		raw := strings.TrimSpace(c.Secrets.Env.Key)
		if raw == "" || strings.HasPrefix(raw, "${") {
			return fmt.Errorf("secrets.env.key: 未设置（请 export THVV_SECRET_KEY）")
		}
		key, err := decodeKey(raw)
		if err != nil {
			return fmt.Errorf("secrets.env.key: %w", err)
		}
		c.Secrets.MasterKey = key
	case "file":
		raw, err := os.ReadFile(string(c.Secrets.File.Path))
		if err != nil {
			return fmt.Errorf("secrets.file.path: %w", err)
		}
		key, err := decodeKey(strings.TrimSpace(string(raw)))
		if err != nil {
			return fmt.Errorf("secrets.file.path: %w", err)
		}
		c.Secrets.MasterKey = key
	default:
		return fmt.Errorf("secrets.provider: 不支持 %q（env | file）", c.Secrets.Provider)
	}
	return nil
}

// ---------- 小工具（供 setByPath 使用） ----------

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("不是整数: %q", s)
	}
	return n, nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("不是布尔值: %q", s)
}

func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("不是时长: %q", s)
	}
	return d, nil
}
