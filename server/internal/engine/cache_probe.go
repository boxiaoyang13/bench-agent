package engine

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// 前缀命中率探针：lmcache bench 的 request_sender 只提取 prompt/completion tokens，
// 不抓 usage.prompt_tokens_details.cached_tokens，CSV 里没有命中数据。
// 探针在 bench 之前直连端点发少量受控请求（1 次冷 + N 次增长），逐轮记录原始 usage，
// 得到「前缀命中率」——比 TTFT 改善率更直接回答「缓存有没有生效」。

const (
	probeGrowthRounds = 4 // 冷启动后追加增长的轮数
	probeMaxTokens    = 8 // 每轮输出上限（只关心 usage，不需要生成）
	probeReqTimeout   = 180 * time.Second
)

// probeUnit 是合成上下文的重复单元；实测约 80 token/次（GLM 分词）。
const probeUnit = "大语言模型缓存技术通过复用已有前缀的键值状态来减少重复计算。" +
	"当多个请求共享相同前缀时，推理引擎只需对新增部分做 prefill，" +
	"从而显著降低首 token 延迟并提高整体吞吐。前缀匹配通常以块为单位，" +
	"常见的块大小为 16 到 256 个 token，不同实现各有取舍。"
const probeUnitTokens = 80

// probePrefixCache 直连端点测量前缀命中率。失败不致命（返回 error，由调用方决定是否记录）。
func (e *CacheEngine) probePrefixCache(ctx context.Context, spec Spec, outDir string) ([]domain.CacheProbeRound, error) {
	p := spec.Plan
	total := p.SharedPromptLen + p.ChatHistoryLen
	if total <= 0 {
		total = 12000
	}
	repeat := int(math.Ceil(float64(total) / probeUnitTokens))
	if repeat < 1 {
		repeat = 1
	}
	basePrefix := strings.Repeat(probeUnit, repeat)
	growth := p.UserInputLen
	if growth <= 0 {
		growth = 50
	}
	growText := strings.Repeat("多轮对话场景中，用户会基于之前的上下文继续提问。", int(math.Ceil(float64(growth)/20))+1)

	rounds := make([]domain.CacheProbeRound, 0, probeGrowthRounds+1)
	history := basePrefix
	for r := 0; r <= probeGrowthRounds; r++ {
		select {
		case <-ctx.Done():
			return rounds, ctx.Err()
		default:
		}
		msgs := []map[string]string{
			{"role": "system", "content": "你是一个缓存测试助手。"},
			{"role": "user", "content": history + "\n\n请只回复：OK"},
		}
		pr, err := e.probeOnce(ctx, spec, msgs)
		if err != nil {
			return rounds, fmt.Errorf("探针第 %d 轮失败: %w", r, err)
		}
		pr.Round = r
		if pr.PromptTokens > 0 {
			pr.HitRate = float64(pr.CachedTokens) / float64(pr.PromptTokens)
		}
		rounds = append(rounds, pr)
		history += growText // 模拟多轮增长：每轮追加 ~user_input_length token
	}

	// 落原始 usage CSV（用户要看的原始数据）
	if len(rounds) > 0 {
		writeProbeCSV(outDir, rounds)
	}
	return rounds, nil
}

// probeOnce 发一条非流式请求，返回原始 usage（prompt/cached tokens + 首包时间）。
func (e *CacheEngine) probeOnce(ctx context.Context, spec Spec, msgs []map[string]string) (domain.CacheProbeRound, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       spec.Target.Model,
		"messages":    msgs,
		"max_tokens":  probeMaxTokens,
		"stream":      false,
		"temperature": 0,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.Target.APIURL, bytes.NewReader(body))
	if err != nil {
		return domain.CacheProbeRound{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+spec.Target.APIKey)

	start := time.Now()
	resp, err := (&http.Client{Timeout: probeReqTimeout}).Do(req)
	if err != nil {
		return domain.CacheProbeRound{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return domain.CacheProbeRound{}, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, raw)
	}
	reqMS := time.Since(start).Seconds() * 1000

	var payload struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.CacheProbeRound{}, fmt.Errorf("usage 解析失败: %w", err)
	}
	if payload.Usage == nil || payload.Usage.PromptTokens == 0 {
		return domain.CacheProbeRound{}, fmt.Errorf("端点未返回 usage（非 OpenAI 兼容响应）")
	}
	return domain.CacheProbeRound{
		PromptTokens: payload.Usage.PromptTokens,
		CachedTokens: payload.Usage.PromptTokensDetails.CachedTokens,
		ReqMS:        math.Round(reqMS*100) / 100,
	}, nil
}

// DeriveProbeHitRate 从探针轮次派生稳态命中率与非首轮统计。
// 返回 (稳态命中率, 端点是否上报过 cached_tokens 字段)。
func DeriveProbeHitRate(rounds []domain.CacheProbeRound) (float64, bool) {
	if len(rounds) < 2 {
		return 0, false
	}
	var cached, prompt int
	reported := false
	for _, r := range rounds[1:] { // 非首轮 = 稳态
		prompt += r.PromptTokens
		cached += r.CachedTokens
		if r.CachedTokens > 0 {
			reported = true
		}
	}
	if prompt == 0 {
		return 0, reported
	}
	return float64(cached) / float64(prompt), reported
}

// writeProbeCSV 落探针的原始 usage（round,prompt_tokens,cached_tokens,hit_rate,req_ms）。
func writeProbeCSV(dir string, rounds []domain.CacheProbeRound) {
	p := filepath.Join(dir, "prefix_usage.csv")
	f, err := os.Create(p)
	if err != nil {
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"round", "prompt_tokens", "cached_tokens", "hit_rate", "req_ms"})
	for _, r := range rounds {
		_ = w.Write([]string{
			strconv.Itoa(r.Round), strconv.Itoa(r.PromptTokens), strconv.Itoa(r.CachedTokens),
			strconv.FormatFloat(r.HitRate, 'f', 4, 64), strconv.FormatFloat(r.ReqMS, 'f', 2, 64),
		})
	}
}
