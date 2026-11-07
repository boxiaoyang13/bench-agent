package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/secret"
)

// ---------------- DTO ----------------

type targetDefaultsDTO struct {
	Temperature       any `json:"temperature,omitempty"`
	MaxTokens         int `json:"max_tokens,omitempty"`
	ConnectTimeoutSec int `json:"connect_timeout_sec,omitempty"`
	ReadTimeoutSec    int `json:"read_timeout_sec,omitempty"`
}

type targetCreateDTO struct {
	Name      string            `json:"name"`
	Vendor    string            `json:"vendor,omitempty"`
	APIURL    string            `json:"api_url"`
	Protocol  string            `json:"protocol"`
	Model     string            `json:"model"`
	APIKey    string            `json:"api_key"`
	Tokenizer *string           `json:"tokenizer,omitempty"`
	Defaults  targetDefaultsDTO `json:"defaults,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type targetPatchDTO struct {
	Name      *string            `json:"name,omitempty"`
	Vendor    *string            `json:"vendor,omitempty"`
	APIURL    *string            `json:"api_url,omitempty"`
	Protocol  *string            `json:"protocol,omitempty"`
	Model     *string            `json:"model,omitempty"`
	APIKey    *string            `json:"api_key,omitempty"`
	Tokenizer *string            `json:"tokenizer,omitempty"`
	Defaults  *targetDefaultsDTO `json:"defaults,omitempty"`
	Labels    *map[string]string `json:"labels,omitempty"`
}

func (s *Server) targetView(t *domain.Target) map[string]any {
	out := map[string]any{
		"target_id":    t.ID,
		"name":         t.Name,
		"vendor":       t.Vendor,
		"api_url":      t.APIURL,
		"protocol":     t.Protocol,
		"model":        t.Model,
		"api_key_hint": t.APIKeyHint,
		"defaults":     t.Defaults,
		"labels":       t.Labels,
		"health":       t.Health,
		"created_at":   t.CreatedAt,
		"updated_at":   t.UpdatedAt,
	}
	if t.Tokenizer != nil {
		out["tokenizer"] = t.Tokenizer
	} else {
		out["tokenizer"] = nil
	}
	return out
}

// ---------------- 校验 ----------------

func (s *Server) validateTargetURL(protocol domain.Protocol, rawURL string) *domain.Problem {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return domain.NewProblem(400, "validation_failed", "api_url 必须以 http(s):// 开头")
	}
	if !s.cfg.Security.AllowPrivateTargets {
		if isPrivateHost(rawURL) {
			return domain.NewProblem(422, "target_unreachable", "不允许访问内网地址（security.allow_private_targets=false）")
		}
	}
	for _, d := range s.cfg.Security.TargetHostDenylist {
		if strings.Contains(rawURL, d) {
			return domain.NewProblem(422, "target_unreachable", "目标地址在拒绝列表内: "+d)
		}
	}
	if len(s.cfg.Security.TargetHostAllowlist) > 0 {
		ok := false
		for _, a := range s.cfg.Security.TargetHostAllowlist {
			if strings.Contains(rawURL, a) {
				ok = true
				break
			}
		}
		if !ok {
			return domain.NewProblem(422, "target_unreachable", "目标地址不在允许列表内")
		}
	}
	switch protocol {
	case domain.ProtocolOpenAI:
		if !strings.HasSuffix(strings.TrimRight(rawURL, "/"), "/chat/completions") {
			return domain.NewProblem(400, "validation_failed", "OpenAI 协议的 api_url 应以 /chat/completions 结尾")
		}
	case domain.ProtocolAnthropic:
		if !strings.HasSuffix(strings.TrimRight(rawURL, "/"), "/messages") {
			return domain.NewProblem(400, "validation_failed", "Anthropic 协议的 api_url 应以 /messages 结尾")
		}
	default:
		return domain.NewProblem(400, "validation_failed", "protocol 必须是 openai 或 anthropic")
	}
	return nil
}

// resolveTokenizer 按配置的 tokenizer_map 推断。
func (s *Server) resolveTokenizer(model, explicit string) domain.TokenizerInfo {
	if explicit != "" {
		return domain.TokenizerInfo{Value: explicit, Source: "explicit", Cached: s.tokenizerCached(explicit)}
	}
	if perf := s.perfCapability(); perf != nil && perf.Defaults != nil {
		for _, r := range perf.Defaults.TokenizerMap {
			if r.Hits(model) {
				return domain.TokenizerInfo{Value: r.Tokenizer, Source: "auto_detected", Cached: s.tokenizerCached(r.Tokenizer)}
			}
		}
	}
	return domain.TokenizerInfo{Source: "fallback"}
}

// tokenizerCached 检查本地缓存目录里是否存在该 tokenizer（粗判，避免启动时就下模型）。
func (s *Server) tokenizerCached(name string) bool {
	if name == "" {
		return false
	}
	base := string(s.cfg.Paths.CacheDir)
	if base == "" {
		return false
	}
	candidates := []string{
		base + "/huggingface/hub/models--" + strings.ReplaceAll(name, "/", "--"),
		base + "/huggingface/" + name,
		// ModelScope 1.38+ 的落盘布局：<cache>/models/<org>--<model>
		base + "/modelscope/models/" + strings.ReplaceAll(name, "/", "--"),
		base + "/modelscope/hub/models/" + strings.ReplaceAll(name, "/", "--"),
		base + "/modelscope/hub/" + name,
		base + "/modelscope/" + name,
	}
	for _, c := range candidates {
		if _, err := statDir(c); err == nil {
			return true
		}
	}
	return false
}

// resolveTemperature 按模型名自适应（kimi/moonshot → 1.0，其余 0.0）。
func (s *Server) resolveTemperature(model string, raw any) any {
	if raw != nil {
		if str, ok := raw.(string); !ok || str != "auto" {
			return raw
		}
	}
	if perf := s.perfCapability(); perf != nil && perf.Defaults != nil {
		for _, r := range perf.Defaults.TemperatureMap {
			if r.Hits(model) {
				return r.Value
			}
		}
	}
	return 0.0
}

// ---------------- handlers ----------------

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var dto targetCreateDTO
	if !s.decode(w, r, &dto) {
		return
	}
	if dto.Name == "" {
		dto.Name = autoName(dto.Vendor, dto.Model)
	}
	if dto.Name == "" || dto.Model == "" || dto.APIKey == "" {
		writeProblem(w, r, domain.NewProblem(400, "validation_failed", "name/model/api_key 必填"))
		return
	}
	protocol := domain.Protocol(orDefault(dto.Protocol, "openai"))
	if p := s.validateTargetURL(protocol, dto.APIURL); p != nil {
		writeProblem(w, r, p)
		return
	}
	if n, _ := s.st.CountTargets(r.Context()); n >= s.cfg.Security.MaxTargets {
		writeProblem(w, r, domain.NewProblem(429, "quota_exceeded", "已登记的端点数达到上限"))
		return
	}

	explicit := ""
	if dto.Tokenizer != nil {
		explicit = *dto.Tokenizer
	}
	tok := s.resolveTokenizer(dto.Model, explicit)

	enc, err := s.box.Encrypt(dto.APIKey)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	now := time.Now().UTC()
	t := &domain.Target{
		ID:         newID("tgt"),
		Name:       dto.Name,
		Vendor:     dto.Vendor,
		APIURL:     dto.APIURL,
		Protocol:   protocol,
		Model:      dto.Model,
		APIKeyHint: secret.Hint(dto.APIKey),
		APIKeyEnc:  enc,
		Tokenizer:  &tok,
		Defaults: domain.TargetDefaults{
			Temperature:       s.resolveTemperature(dto.Model, dto.Defaults.Temperature),
			MaxTokens:         defaultInt(dto.Defaults.MaxTokens, s.defaultMaxTokens()),
			ConnectTimeoutSec: defaultInt(dto.Defaults.ConnectTimeoutSec, 60),
			ReadTimeoutSec:    defaultInt(dto.Defaults.ReadTimeoutSec, 600),
		},
		Labels:    dto.Labels,
		Health:    domain.TargetHealth{Status: "unknown"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.st.CreateTarget(r.Context(), t); err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "target.create", t.ID, t.Name)
	writeJSON(w, http.StatusCreated, s.targetView(t))
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	items, err := s.st.ListTargets(r.Context(), r.URL.Query().Get("vendor"), limit)
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, t := range items {
		out = append(out, s.targetView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "has_more": false, "next_cursor": nil})
}

func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.GetTarget(r.Context(), r.PathValue("target_id"))
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, s.targetView(t))
}

func (s *Server) handlePatchTarget(w http.ResponseWriter, r *http.Request) {
	var dto targetPatchDTO
	if !s.decode(w, r, &dto) {
		return
	}
	t, err := s.st.GetTarget(r.Context(), r.PathValue("target_id"))
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	if dto.Name != nil {
		t.Name = *dto.Name
	}
	if dto.Vendor != nil {
		t.Vendor = *dto.Vendor
	}
	if dto.APIURL != nil {
		t.APIURL = *dto.APIURL
	}
	if dto.Protocol != nil {
		t.Protocol = domain.Protocol(*dto.Protocol)
	}
	if dto.Model != nil {
		t.Model = *dto.Model
	}
	if p := s.validateTargetURL(t.Protocol, t.APIURL); p != nil {
		writeProblem(w, r, p)
		return
	}
	if dto.APIKey != nil && *dto.APIKey != "" {
		enc, err := s.box.Encrypt(*dto.APIKey)
		if err != nil {
			writeProblem(w, r, errProblem(err))
			return
		}
		t.APIKeyEnc = enc
		t.APIKeyHint = secret.Hint(*dto.APIKey)
	}
	if dto.Tokenizer != nil {
		tok := s.resolveTokenizer(t.Model, *dto.Tokenizer)
		t.Tokenizer = &tok
	}
	if dto.Defaults != nil {
		if dto.Defaults.Temperature != nil {
			t.Defaults.Temperature = s.resolveTemperature(t.Model, dto.Defaults.Temperature)
		}
		if dto.Defaults.MaxTokens > 0 {
			t.Defaults.MaxTokens = dto.Defaults.MaxTokens
		}
	}
	if dto.Labels != nil {
		t.Labels = *dto.Labels
	}
	t.UpdatedAt = time.Now().UTC()
	if err := s.st.UpdateTarget(r.Context(), t); err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "target.update", t.ID, t.Name)
	writeJSON(w, http.StatusOK, s.targetView(t))
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("target_id")
	if err := s.st.DeleteTarget(r.Context(), id); err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	s.st.Audit(r.Context(), actorName(r), "target.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}

type probeRequest struct {
	Checks     []string `json:"checks,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

func (s *Server) handleProbeTarget(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if r.ContentLength > 0 {
		if !s.decode(w, r, &req) {
			return
		}
	}
	t, err := s.st.GetTarget(r.Context(), r.PathValue("target_id"))
	if err != nil {
		writeProblem(w, r, errProblem(err))
		return
	}
	key, _ := s.box.Decrypt(t.APIKeyEnc)
	tok := ""
	if t.Tokenizer != nil {
		tok = t.Tokenizer.Value
	}
	rt := engine.ResolvedTarget{
		TargetID: t.ID, Vendor: t.Vendor, Model: t.Model, APIURL: t.APIURL,
		Protocol: t.Protocol, APIKey: key, Tokenizer: tok,
	}
	timeout := time.Duration(defaultInt(req.TimeoutSec, 30)) * time.Second
	ctx, cancel := contextWithTimeout(r, timeout)
	defer cancel()

	// ?capability=cache/eval 时用对应引擎预检
	probeCap := domain.Capability(orDefault(r.URL.Query().Get("capability"), string(domain.CapPerf)))
	var eng engine.Engine
	if e, err := s.reg.Get(s.reg.ToolForCap(probeCap), probeCap); err == nil {
		eng = e
	} else {
		eng = s.reg.AnyProbe(s.toolOf(t))
	}
	if eng == nil {
		writeProblem(w, r, domain.NewProblem(501, "not_implemented", "没有可用的引擎做预检"))
		return
	}
	rep := eng.Probe(ctx, rt)

	// 数据集检查只对 perf 有意义；eval/cache 的数据集/环境由各自引擎自查
	if probeCap == domain.CapPerf {
		rep.Checks = s.augmentDatasetCheck(rep.Checks)
	}

	ok := true
	sev := "ok"
	for _, c := range rep.Checks {
		if !c.OK {
			if c.Severity == "warn" {
				if sev == "ok" {
					sev = "warn"
				}
			} else {
				ok = false
				sev = "error"
			}
		}
	}
	rep.OK = ok
	checkedAt := time.Now().UTC()
	t.Health = domain.TargetHealth{Status: sev, CheckedAt: &checkedAt}
	t.UpdatedAt = checkedAt
	_ = s.st.UpdateTarget(r.Context(), t)

	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) augmentDatasetCheck(checks []engine.CheckItem) []engine.CheckItem {
	perf := s.perfCapability()
	if perf == nil {
		return checks
	}
	var missing []string
	for _, b := range perf.Buckets {
		builtIn := b.DatasetBuiltIn == nil || *b.DatasetBuiltIn
		if !builtIn {
			missing = append(missing, b.Label)
			continue
		}
		if !fileExists(datasetPath(string(s.cfg.Paths.DatasetsDir), b.Label)) {
			missing = append(missing, b.Label)
		}
	}
	out := make([]engine.CheckItem, 0, len(checks)+1)
	for _, c := range checks {
		if c.Name == "datasets" {
			continue
		}
		out = append(out, c)
	}
	if len(missing) == 0 {
		out = append(out, engine.CheckItem{Name: "datasets", OK: true, Detail: "全部档位数据集就绪"})
	} else {
		out = append(out, engine.CheckItem{
			Name: "datasets", OK: false, Severity: "warn",
			Detail:  "以下档位数据集缺失或未构建（不影响其余档位）",
			Missing: missing,
		})
	}
	return out
}

// toolOf 决定用哪个引擎做 probe（默认第一个 enabled 的）。
func (s *Server) toolOf(t *domain.Target) string {
	if len(s.cfg.Engines) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.cfg.Engines))
	for n := range s.cfg.Engines {
		names = append(names, n)
	}
	sortStrings(names)
	return names[0]
}

func (s *Server) defaultMaxTokens() int {
	if perf := s.perfCapability(); perf != nil && perf.Defaults != nil && perf.Defaults.MaxTokens > 0 {
		return perf.Defaults.MaxTokens
	}
	return 4096
}

// ---------------- helpers ----------------

func autoName(vendor, model string) string {
	if model == "" {
		return ""
	}
	name := strings.ToLower(model)
	repl := strings.NewReplacer(" ", "-", "_", "-", "/", "-", ".", "-", ":", "-")
	name = repl.Replace(name)
	if vendor != "" {
		return strings.ToLower(vendor) + "-" + name
	}
	return name
}

func defaultInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func datasetPath(dir, bucket string) string {
	return dir + "/perf_zh_" + bucket + ".jsonl"
}

func actorName(r *http.Request) string {
	if p := principalOf(r); p != nil {
		return p.Name
	}
	return "anonymous"
}
