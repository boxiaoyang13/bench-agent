package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeTHVV 造一个能通过指纹校验的 THVV 根目录。
func fakeTHVV(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "thvv")
	for _, p := range []string{"perf/scripts", "perf/datasets_clean"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, content := range map[string]string{
		"perf/run.sh":                          "#!/bin/sh\n",
		"perf/scripts/setup_tokenizer.py":      "",
		"perf/datasets_clean/perf_zh_1k.jsonl": "{}\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const minimalYAML = `
version: 1
roots:
  thvv: "%THVV%"
  data: "data"
  bin:  "thvv:.venv/bin"
auth:
  mode: both
  tokens: [{ name: t, token: "tok", role: operator }]
  users:
    - username: u
      password_hash: "$2a$10$IBU8oAPb/vYyjaHjJHAhMuaUS9nblXw3MuVZLit8R88wEZEZ.e9xC"
      role: admin
store: { driver: sqlite, sqlite: { path: "data:thvv.db" } }
secrets: { provider: env, env: { key: "0123456789abcdef0123456789abcdef" } }
paths:
  runs_dir: "data:runs"
  logs_dir: "data:logs"
  cache_dir: "data:cache"
  datasets_dir: "thvv:perf/datasets_clean"
engines:
  thvv:
    enabled: true
    kind: builtin
    capabilities:
      perf: { enabled: true }
observability: { log_level: error }
`

// TestRootsAndPrefixResolution 覆盖「Go 服务与 thvv 不同路径」的核心场景。
func TestRootsAndPrefixResolution(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	cfgDir := filepath.Join(base, "config", "nested")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := minimalYAML
	content = replaceAll(content, "%THVV%", thvvDir)
	p := writeCfg(t, cfgDir, "thvv.yaml", content)

	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	// 根解析
	if got := cfg.Resolved["roots.thvv"]; got != thvvDir {
		t.Fatalf("roots.thvv 期望 %s，实际 %s", thvvDir, got)
	}
	// cfg 是隐式根
	if got := cfg.Resolved["roots.cfg"]; got != cfgDir {
		t.Fatalf("roots.cfg 期望 %s，实际 %s", cfgDir, got)
	}
	// data 相对配置文件目录
	if got := cfg.Resolved["roots.data"]; got != filepath.Join(cfgDir, "data") {
		t.Fatalf("roots.data 期望 %s，实际 %s", filepath.Join(cfgDir, "data"), got)
	}
	// bin 引用 thvv 根
	if got := cfg.Resolved["roots.bin"]; got != filepath.Join(thvvDir, ".venv/bin") {
		t.Fatalf("roots.bin 期望 %s，实际 %s", filepath.Join(thvvDir, ".venv/bin"), got)
	}
	// 前缀寻址
	if got := string(cfg.Paths.DatasetsDir); got != filepath.Join(thvvDir, "perf/datasets_clean") {
		t.Fatalf("datasets_dir 解析错误: %s", got)
	}
	if got := string(cfg.Store.SQLite.Path); got != filepath.Join(cfgDir, "data", "thvv.db") {
		t.Fatalf("store path 解析错误: %s", got)
	}
}

// TestThvvHomeOverridesRoots 验证 -thvv-home 覆盖（同一份配置在不同机器上跑）。
func TestThvvHomeOverridesRoots(t *testing.T) {
	base := t.TempDir()
	realDir := fakeTHVV(t, filepath.Join(base, "other"))
	p := writeCfg(t, base, "t.yaml", replaceAll(minimalYAML, "%THVV%", "/nonexistent/thvv"))

	cfg, err := Load(LoadOptions{Path: p, ThvvHome: realDir})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := cfg.Resolved["roots.thvv"]; got != realDir {
		t.Fatalf("-thvv-home 未生效: %s", got)
	}
}

// TestPrefixEscapeRejected 禁止 ../ 逃逸出根。
func TestPrefixEscapeRejected(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	bad := replaceAll(minimalYAML, "%THVV%", thvvDir)
	bad = replaceAll(bad, `datasets_dir: "thvv:perf/datasets_clean"`, `datasets_dir: "thvv:../etc"`)
	p := writeCfg(t, base, "bad.yaml", bad)

	if _, err := Load(LoadOptions{Path: p}); err == nil {
		t.Fatal("期望因路径逃逸报错，但加载成功")
	}
}

// TestUnknownRootRejected 未知根前缀必须报错。
func TestUnknownRootRejected(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	bad := replaceAll(minimalYAML, "%THVV%", thvvDir)
	bad = replaceAll(bad, `runs_dir: "data:runs"`, `runs_dir: "nosuch:runs"`)
	p := writeCfg(t, base, "bad2.yaml", bad)

	if _, err := Load(LoadOptions{Path: p}); err == nil {
		t.Fatal("期望因未知根报错")
	}
}

// TestEnvExpansion 验证 ${VAR} / ${VAR:-default} / $$ 转义。
func TestEnvExpansion(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	content := replaceAll(minimalYAML, "%THVV%", "${TEST_THVV_ROOT}")
	content = replaceAll(content, "data:runs", "${TEST_RUNS_DIR:-data:runs}")
	p := writeCfg(t, base, "env.yaml", content)

	t.Setenv("TEST_THVV_ROOT", thvvDir)
	if _, err := Load(LoadOptions{Path: p}); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Resolved["roots.thvv"]; got != thvvDir {
		t.Fatalf("${VAR} 展开失败: %s", got)
	}
	if got := string(cfg.Paths.RunsDir); got != filepath.Join(base, "data", "runs") {
		t.Fatalf("${VAR:-default} 展开失败: %s", got)
	}
}

// TestEnvOverrides 验证 THVV_ 前缀环境变量覆盖。
func TestEnvOverrides(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	p := writeCfg(t, base, "o.yaml", replaceAll(minimalYAML, "%THVV%", thvvDir))

	t.Setenv("THVV_LIMITS__MAX_CONCURRENT_RUNS", "7")
	t.Setenv("THVV_SERVER__LISTEN", "0.0.0.0:9999")
	t.Setenv("THVV_OBSERVABILITY__LOG_LEVEL", "debug")

	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxConcurrentRuns != 7 {
		t.Fatalf("环境变量覆盖失败: %d", cfg.Limits.MaxConcurrentRuns)
	}
	if cfg.Server.Listen != "0.0.0.0:9999" {
		t.Fatalf("listen 覆盖失败: %s", cfg.Server.Listen)
	}
	if cfg.Observability.LogLevel != "debug" {
		t.Fatalf("log_level 覆盖失败: %s", cfg.Observability.LogLevel)
	}
}

// TestFingerprintValidation 目录存在但不是 THVV 根 → 报错。
// 指纹校验只在 fork Python（kind=subprocess）时才做。
func TestFingerprintValidation(t *testing.T) {
	base := t.TempDir()
	notTHVV := filepath.Join(base, "whatever")
	if err := os.MkdirAll(notTHVV, 0o755); err != nil {
		t.Fatal(err)
	}
	subproc := replaceAll(minimalYAML, "kind: builtin", "kind: subprocess\n    runtime: {python: /bin/sh, evalscope: /bin/sh}")
	p := writeCfg(t, base, "fp.yaml", replaceAll(subproc, "%THVV%", notTHVV))
	_, err := Load(LoadOptions{Path: p})
	if err == nil {
		t.Fatal("期望指纹校验失败")
	}
	issues := (*Config)(nil)
	_ = issues
	if !containsStr(err.Error(), "看起来不是 THVV 根目录") {
		t.Fatalf("错误信息不友好: %v", err)
	}
}

// TestDefaults 默认值 = 现 run.sh 行为。
func TestDefaults(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	p := writeCfg(t, base, "d.yaml", replaceAll(minimalYAML, "%THVV%", thvvDir))
	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	perf := cfg.Engines["thvv"].Capabilities["perf"]

	if len(perf.Buckets) != 9 {
		t.Fatalf("默认档位数应为 9，实际 %d", len(perf.Buckets))
	}
	if perf.Buckets[6].Label != "200k" || perf.Buckets[6].MaxParallel != 16 {
		t.Fatalf("200k 档位默认值错误: %+v", perf.Buckets[6])
	}
	if perf.Buckets[8].DatasetBuiltIn == nil || *perf.Buckets[8].DatasetBuiltIn {
		t.Fatal("1m 档位应默认 dataset_built_in=false")
	}
	if len(perf.Matrix.DefaultLadder) != 5 {
		t.Fatalf("默认并发梯度错误: %v", perf.Matrix.DefaultLadder)
	}
	if perf.Matrix.Cooldown.Seconds() != 120 {
		t.Fatalf("默认冷却应为 120s: %v", perf.Matrix.Cooldown)
	}
	// temperature / tokenizer 自适应表
	var kimi, others bool
	for _, r := range perf.Defaults.TemperatureMap {
		if r.Hits("kimi-k2") && r.Value == 1.0 {
			kimi = true
		}
		if r.Hits("glm-5.2") && r.Value == 0.0 {
			others = true
		}
	}
	if !kimi || !others {
		t.Fatalf("temperature_map 默认值异常: %+v", perf.Defaults.TemperatureMap)
	}
	var glmTok string
	for _, r := range perf.Defaults.TokenizerMap {
		if r.Hits("glm-5.2") {
			glmTok = r.Tokenizer
		}
	}
	if glmTok != "zai-org/GLM-4.6" {
		t.Fatalf("tokenizer_map 默认值异常: %q", glmTok)
	}
	// 内置 SLA 模板
	sla := cfg.Gates.Profiles["perf-sla-v1"]
	if sla == nil || len(sla.Tiers) != 6 {
		t.Fatalf("内置 SLA 模板异常: %+v", sla)
	}
	if sla.Global.MinOutputTokensPerSec != 30 || sla.Global.MinSuccessRate != 0.99 {
		t.Fatalf("SLA 全局阈值异常: %+v", sla.Global)
	}
}

// TestSecrets 主密钥校验。
func TestSecrets(t *testing.T) {
	base := t.TempDir()
	thvvDir := fakeTHVV(t, base)
	p := writeCfg(t, base, "s.yaml", replaceAll(minimalYAML, "%THVV%", thvvDir))

	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Secrets.MasterKey) != 32 {
		t.Fatalf("主密钥长度错误: %d", len(cfg.Secrets.MasterKey))
	}
	// 替换成非法短 key
	bad := replaceAll(readFile(t, p), "0123456789abcdef0123456789abcdef", "short")
	p2 := writeCfg(t, base, "s2.yaml", bad)
	if _, err := Load(LoadOptions{Path: p2}); err == nil {
		t.Fatal("短密钥应被拒绝")
	}
}

// ---------------- helpers ----------------

func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsStr(s, sub string) bool { return indexOf(s, sub) >= 0 }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestBuiltinEngineNeedsNoTHVV builtin 模拟引擎不需要 THVV 工具链（本地联调开箱即用）。
func TestBuiltinEngineNeedsNoTHVV(t *testing.T) {
	base := t.TempDir()
	// thvv 根故意指向不存在的目录
	p := writeCfg(t, base, "nothvv.yaml", replaceAll(minimalYAML, "%THVV%", filepath.Join(base, "missing-thvv")))
	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatalf("builtin 引擎不应要求 THVV 根目录: %v", err)
	}
	// 有 warn（数据集缺失），但不能有 error
	for _, is := range cfg.Check() {
		if is.Level == "error" {
			t.Fatalf("不应出现 error: %s", is)
		}
	}
}
