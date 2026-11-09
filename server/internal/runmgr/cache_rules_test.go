package runmgr

import (
	"strings"
	"testing"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// TestDeriveCacheHitRateShape 目标命中率 → 每轮输入/输出长度 的反推口径。
func TestDeriveCacheHitRateShape(t *testing.T) {
	base := func() domain.Plan {
		return domain.Plan{
			Workload:        "multi-round-chat",
			SharedPromptLen: 2000, ChatHistoryLen: 10000, // base = 12000
		}
	}

	// 下限 80%：new = 12000 × 0.2/0.8 = 3000 → 输出 200、输入 2800
	p := base()
	p.HitRateMin = 0.80
	if err := deriveCacheHitRateShape(&p); err != nil {
		t.Fatalf("min 80%% 反推失败: %v", err)
	}
	if p.OutputLen != 200 || p.UserInputLen != 2800 {
		t.Fatalf("min 80%% 期望 输入 2800 / 输出 200，实际 %d/%d", p.UserInputLen, p.OutputLen)
	}

	// 上限 80%：目标取 77% → new = 12000 × 0.23/0.77 ≈ 3584
	p = base()
	p.HitRateMax = 0.80
	if err := deriveCacheHitRateShape(&p); err != nil {
		t.Fatalf("max 80%% 反推失败: %v", err)
	}
	if p.OutputLen != 200 || p.UserInputLen != 3384 {
		t.Fatalf("max 80%% 期望 输入 3384 / 输出 200，实际 %d/%d", p.UserInputLen, p.OutputLen)
	}

	// 极高目标：new 很小，输出自动收缩，输入至少 1
	p = base()
	p.HitRateMin = 0.995
	if err := deriveCacheHitRateShape(&p); err != nil {
		t.Fatalf("99.5%% 反推失败: %v", err)
	}
	if p.UserInputLen < 1 || p.OutputLen < 1 || p.UserInputLen+p.OutputLen != 60 {
		t.Fatalf("99.5%% 期望 输入+输出=60，实际 %d+%d", p.UserInputLen, p.OutputLen)
	}

	// 互斥：min + max 同时设置 → 报错
	p = base()
	p.HitRateMin, p.HitRateMax = 0.8, 0.9
	if err := deriveCacheHitRateShape(&p); err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("min+max 应报互斥错误，实际 %v", err)
	}

	// 缺 shared/history → 报错
	p = domain.Plan{Workload: "multi-round-chat", HitRateMin: 0.8}
	if err := deriveCacheHitRateShape(&p); err == nil {
		t.Fatal("缺 shared+history 应报错")
	}

	// 目标过低 → 每轮 token 超 1M → 报错
	p = base()
	p.HitRateMin = 0.01
	if err := deriveCacheHitRateShape(&p); err == nil {
		t.Fatal("命中率 1% 反推 1M+ token 应报错")
	}

	// 越界目标 → 报错
	p = base()
	p.HitRateMin = 1.0
	if err := deriveCacheHitRateShape(&p); err == nil {
		t.Fatal("命中率 100% 应报错")
	}
}
