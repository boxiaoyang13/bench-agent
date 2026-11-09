package runmgr

import (
	"context"
	"errors"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/store"
)

// ErrEphemeralRerun 表示该 run 用的是内联 target（凭据未持久化），无法重测。
var ErrEphemeralRerun = errors.New("该 run 使用内联 target（凭据未持久化），无法重测；请重新提交")

// Rerun 用同一条记录的原配置（target + capability + plan + gate + report）新建一个 run。
//
// 用途：历史页「重测」按钮 —— 失败的记录一键重跑，不用在向导里把参数再填一遍。
// 规则：
//   - 只能重测终态 run（运行中的先取消）；
//   - target 必须还在（被删除的 target 会明确报错）；
//   - judge 凭据从 runs.judge_key_enc 解回明文后重新加密落到新 run，明文不出内存。
func (m *Manager) Rerun(ctx context.Context, runID, createdBy string) (*domain.Run, error) {
	old, err := m.st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !old.Status.Terminal() {
		return nil, invalid("run 当前状态为 %s，请先取消并等待进入终态后再重测", old.Status)
	}
	if old.Target.Ephemeral || old.Target.TargetID == "" || old.Target.TargetID == "tgt-ephemeral" {
		return nil, ErrEphemeralRerun
	}

	target, err := m.st.GetTarget(ctx, old.Target.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, invalid("原端点 %s 已被删除，无法重测；请重新创建端点后提交", old.Target.TargetID)
		}
		return nil, err
	}
	apiKey, err := m.box.Decrypt(target.APIKeyEnc)
	if err != nil {
		return nil, err
	}

	plan := old.Plan
	plan.StepsTotal = 0 // 由 Submit 重新计算
	if plan.Judge != nil {
		// 库里只存掩码，明文要单独解回来，否则 Judge 数据集重测会 400
		plain := ""
		if len(old.JudgeKeyEnc) > 0 {
			if plain, err = m.box.Decrypt(old.JudgeKeyEnc); err != nil {
				return nil, err
			}
		}
		plan.Judge.APIKey = plain
	}

	labels := map[string]string{}
	for k, v := range old.Labels {
		labels[k] = v
	}
	labels["rerun_of"] = old.ID

	run, err := m.Submit(ctx, SubmitRequest{
		Tool:       old.Tool,
		Capability: old.Capability,
		Target:     target,
		APIKey:     apiKey,
		Plan:       plan,
		Gate:       old.Gate,
		ReportSpec: old.Report,
		Labels:     labels,
		CreatedBy:  createdBy,
	})
	if err != nil {
		return nil, err
	}
	m.log.Info("run rerun", "from", old.ID, "to", run.ID, "cap", old.Capability)
	return run, nil
}
