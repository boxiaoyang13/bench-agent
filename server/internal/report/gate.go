package report

import (
	"fmt"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/domain"
)

// AvgPromptTokens 返回该 step 的平均输入 token（用于分档）。
// 口径与 sla_eval.py 一致：成功请求的 prompt_tokens 均值。
func AvgPromptTokens(m domain.StepMetrics) float64 {
	if m.Requests.Success == 0 {
		return 0
	}
	return float64(m.Tokens.PromptSum) / float64(m.Requests.Success)
}

// ClassifyTier 按平均输入长度归档（lo <= x < hi，上界不含）。
func ClassifyTier(avgPromptTokens float64, p *config.GateProfile) *config.GateTier {
	if p == nil || avgPromptTokens <= 0 {
		return nil
	}
	for i := range p.Tiers {
		t := &p.Tiers[i]
		if avgPromptTokens >= float64(t.Lo) && avgPromptTokens < float64(t.Hi) {
			return t
		}
	}
	return nil
}

// EvaluateGate 做 SLA 判定。
//
// 规则（对齐 sla_eval.py）：
//   - 每个 step 按 avgPromptTokens 归档
//   - 成功率低于全局下限的 step **不参与档位合并统计**（否则污染 P50/P90）
//   - 分档比对 TTFT P50 / P90
//   - 全局比对输出 token 速率
func EvaluateGate(profile *config.GateProfile, steps []domain.StepMetrics) *domain.GateResult {
	res := &domain.GateResult{Passed: true, Violations: []domain.Violation{}, TierSummary: []domain.TierSummary{}}
	if profile == nil {
		return res
	}
	res.Profile = ""

	type agg struct {
		p50 []float64
		p90 []float64
		n   int
	}
	byTier := map[string]*agg{}
	var tierOrder []string

	for _, m := range steps {
		if m.Requests.Total == 0 {
			continue
		}
		if profile.Global.MinSuccessRate > 0 && m.Requests.SuccessRate < profile.Global.MinSuccessRate {
			res.Violations = append(res.Violations, domain.Violation{
				Scope: "step", Bucket: m.Bucket, Parallel: m.Parallel, Rule: "min_success_rate",
				Actual: round(m.Requests.SuccessRate, 4), Threshold: profile.Global.MinSuccessRate,
				Message: fmt.Sprintf("%s/并发%d 成功率 %.2f%% < %.2f%%", m.Bucket, m.Parallel,
					m.Requests.SuccessRate*100, profile.Global.MinSuccessRate*100),
			})
			continue
		}
		tier := ClassifyTier(AvgPromptTokens(m), profile)
		if tier != nil {
			if _, ok := byTier[tier.Tier]; !ok {
				byTier[tier.Tier] = &agg{}
				tierOrder = append(tierOrder, tier.Tier)
			}
			a := byTier[tier.Tier]
			a.p50 = append(a.p50, m.TTFT.P50Sec)
			a.p90 = append(a.p90, m.TTFT.P90Sec)
			a.n++
		}
		if profile.Global.MinOutputTokensPerSec > 0 && m.TokenRateAvg > 0 &&
			m.TokenRateAvg < profile.Global.MinOutputTokensPerSec {
			res.Violations = append(res.Violations, domain.Violation{
				Scope: "step", Bucket: m.Bucket, Parallel: m.Parallel, Rule: "min_output_tokens_per_sec",
				Actual: round(m.TokenRateAvg, 2), Threshold: profile.Global.MinOutputTokensPerSec,
				Message: fmt.Sprintf("%s/并发%d 输出速率 %.1f t/s < %.1f t/s", m.Bucket, m.Parallel,
					m.TokenRateAvg, profile.Global.MinOutputTokensPerSec),
			})
		}
	}

	for _, tier := range profile.Tiers {
		a, ok := byTier[tier.Tier]
		ts := domain.TierSummary{Tier: tier.Tier, Passed: true}
		if ok && a.n > 0 {
			p50Max, _ := minmax(a.p50)
			p90Max, _ := minmax(a.p90)
			ts.Steps = a.n
			ts.P50MaxSec = round(p50Max, 3)
			ts.P90MaxSec = round(p90Max, 3)
			if tier.TTFTp50Max > 0 && p50Max > tier.TTFTp50Max {
				ts.Passed = false
				res.Violations = append(res.Violations, domain.Violation{
					Scope: "tier", Tier: tier.Tier, Rule: "ttft_p50_max",
					Actual: round(p50Max, 3), Threshold: tier.TTFTp50Max,
					Message: fmt.Sprintf("档位 %s P50 TTFT %.2fs > %.2fs", tier.Tier, p50Max, tier.TTFTp50Max),
				})
			}
			if tier.TTFTp90Max > 0 && p90Max > tier.TTFTp90Max {
				ts.Passed = false
				res.Violations = append(res.Violations, domain.Violation{
					Scope: "tier", Tier: tier.Tier, Rule: "ttft_p90_max",
					Actual: round(p90Max, 3), Threshold: tier.TTFTp90Max,
					Message: fmt.Sprintf("档位 %s P90 TTFT %.2fs > %.2fs", tier.Tier, p90Max, tier.TTFTp90Max),
				})
			}
		}
		res.TierSummary = append(res.TierSummary, ts)
	}
	_ = tierOrder
	res.Passed = len(res.Violations) == 0
	return res
}

// BuildSummary 生成 /results 的 summary 块。
func BuildSummary(steps []domain.Step, metrics []domain.StepMetrics, gate *domain.GateResult) map[string]any {
	sum := map[string]any{}
	var okCount, failCount int
	for _, s := range steps {
		switch s.Status {
		case domain.StepSucceeded:
			okCount++
		case domain.StepFailed:
			failCount++
		}
	}
	sum["steps_total"] = len(steps)
	sum["steps_succeeded"] = okCount
	sum["steps_failed"] = failCount

	var totReq, totOK int
	var wP50, wP90, wTPOT float64
	var peakTPS, peakTPM float64
	for _, m := range metrics {
		totReq += m.Requests.Total
		totOK += m.Requests.Success
		w := float64(m.Requests.Success)
		wP50 += m.TTFT.P50Sec * w
		wP90 += m.TTFT.P90Sec * w
		wTPOT += m.TPOT.AvgMs * w
		peakTPS = max(peakTPS, m.Throughput.OutputTPS)
		peakTPM = max(peakTPM, m.Throughput.TPM)
	}
	if totReq > 0 {
		sum["success_rate"] = round(float64(totOK)/float64(totReq), 6)
	}
	if totOK > 0 {
		sum["ttft_p50_sec"] = round(wP50/float64(totOK), 3)
		sum["ttft_p90_sec"] = round(wP90/float64(totOK), 3)
		sum["tpot_ms"] = round(wTPOT/float64(totOK), 3)
	}
	sum["peak_output_tps"] = round(peakTPS, 2)
	sum["tpm_peak"] = round(peakTPM, 1)

	verdict := "pass"
	if failCount > 0 || (gate != nil && !gate.Passed) {
		verdict = "conditional_pass"
	}
	if okCount == 0 && failCount > 0 {
		verdict = "fail"
	}
	sum["verdict"] = verdict
	return sum
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
