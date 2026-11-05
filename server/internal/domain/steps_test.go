package domain

import "testing"

func normFixture() Normalizer {
	return Normalizer{
		Buckets: []Bucket{
			{Label: "1k", MaxParallel: 128, BuiltIn: true, PromptTokens: struct {
				Min int `json:"min"`
				Max int `json:"max"`
			}{870, 1228}},
			{Label: "9k", MaxParallel: 128, BuiltIn: true, PromptTokens: struct {
				Min int `json:"min"`
				Max int `json:"max"`
			}{8000, 11000}},
			{Label: "64k", MaxParallel: 64, BuiltIn: true, PromptTokens: struct {
				Min int `json:"min"`
				Max int `json:"max"`
			}{56000, 72000}},
			{Label: "128k", MaxParallel: 32, BuiltIn: true, PromptTokens: struct {
				Min int `json:"min"`
				Max int `json:"max"`
			}{112000, 144000}},
			{Label: "200k", MaxParallel: 16, BuiltIn: true, PromptTokens: struct {
				Min int `json:"min"`
				Max int `json:"max"`
			}{180000, 220000}},
		},
		DefaultLadder:    []int{1, 8, 32, 64, 128},
		NumberByParallel: map[int]int{1: 20, 8: 60, 32: 120, 64: 200, 128: 400},
		DefaultNumber:    100,
	}
}

// TestComputeStepsParity 的期望值与前端 web/app.js 的 computeSteps() 一致，
// 且与 thvv/perf/run.sh 的并发裁剪规则一致。
func TestComputeStepsParity(t *testing.T) {
	n := normFixture()

	t.Run("单档", func(t *testing.T) {
		steps, skipped := ComputeSteps(Plan{
			Mode: "single", Buckets: []string{"1k"}, ParallelLadder: []int{10}, Number: 200,
		}, n)
		if len(skipped) != 0 {
			t.Fatalf("不应跳过: %+v", skipped)
		}
		if len(steps) != 1 {
			t.Fatalf("步数应为 1，实际 %d", len(steps))
		}
		if steps[0].Number != 200 || steps[0].Parallel != 10 {
			t.Fatalf("step 参数错误: %+v", steps[0])
		}
		if steps[0].MinLen != 870 || steps[0].MaxLen != 1228 {
			t.Fatalf("token 区间未带入: %+v", steps[0])
		}
	})

	t.Run("全档位按并发上限裁剪", func(t *testing.T) {
		steps, _ := ComputeSteps(Plan{
			Mode:    "matrix",
			Buckets: []string{"1k", "9k", "64k", "128k", "200k"},
		}, n)
		want := map[string]int{"1k": 5, "9k": 5, "64k": 4, "128k": 3, "200k": 2}
		got := map[string]int{}
		for _, s := range steps {
			got[s.Bucket]++
		}
		for b, w := range want {
			if got[b] != w {
				t.Errorf("档位 %s 步数期望 %d，实际 %d", b, w, got[b])
			}
		}
		if len(steps) != 19 {
			t.Fatalf("总步数期望 19，实际 %d", len(steps))
		}
	})

	t.Run("TPM 预算裁剪并发", func(t *testing.T) {
		// 100000 / 1228 = 81 → 1,8,32,64 命中（128 被裁掉）
		steps, _ := ComputeSteps(Plan{
			Mode: "matrix", Buckets: []string{"1k"}, TPMBudget: 100000,
		}, n)
		if len(steps) != 4 {
			t.Fatalf("期望 4 步，实际 %d: %+v", len(steps), steps)
		}
		for _, s := range steps {
			if s.Parallel > 81 {
				t.Fatalf("并发 %d 超过 TPM 裁剪上限", s.Parallel)
			}
		}
	})

	t.Run("预算过低跳过整档", func(t *testing.T) {
		steps, skipped := ComputeSteps(Plan{
			Mode: "matrix", Buckets: []string{"200k"}, TPMBudget: 100000,
		}, n)
		if len(steps) != 0 {
			t.Fatalf("应全部跳过，实际 %d 步", len(steps))
		}
		if len(skipped) != 1 || skipped[0].Reason != "budget_too_low" {
			t.Fatalf("跳过原因错误: %+v", skipped)
		}
	})

	t.Run("档位覆盖追加并发", func(t *testing.T) {
		steps, _ := ComputeSteps(Plan{
			Mode:    "matrix",
			Buckets: []string{"200k"},
			BucketOverrides: map[string]BucketOverride{
				"200k": {Parallels: []int{8, 16}, Number: 16},
			},
		}, n)
		got := []int{}
		for _, s := range steps {
			got = append(got, s.Parallel)
			if s.Number != 16 {
				t.Fatalf("档位覆盖的请求数应对该档所有并发生效: %+v", s)
			}
		}
		if len(got) != 3 || got[0] != 1 || got[1] != 8 || got[2] != 16 {
			t.Fatalf("并集去重排序错误: %v", got)
		}
	})

	t.Run("未知档位", func(t *testing.T) {
		_, skipped := ComputeSteps(Plan{Mode: "matrix", Buckets: []string{"999k"}}, n)
		if len(skipped) != 1 || skipped[0].Reason != "unknown_bucket" {
			t.Fatalf("应报 unknown_bucket: %+v", skipped)
		}
	})

	t.Run("请求数按并发映射", func(t *testing.T) {
		steps, _ := ComputeSteps(Plan{Mode: "matrix", Buckets: []string{"1k"}}, n)
		for _, s := range steps {
			if want := n.NumberByParallel[s.Parallel]; want != 0 && s.Number != want {
				t.Errorf("并发 %d 的请求数期望 %d，实际 %d", s.Parallel, want, s.Number)
			}
		}
	})
}

func TestRunStatus(t *testing.T) {
	if !StatusSucceeded.Terminal() || !StatusPartial.Terminal() || !StatusCanceled.Terminal() {
		t.Fatal("终态判定错误")
	}
	if StatusRunning.Terminal() || StatusQueued.Terminal() {
		t.Fatal("非终态被判定为终态")
	}
	// 未知状态必须被视为非终态（协议兼容约定）
	if RunStatus("weird_future_state").Terminal() {
		t.Fatal("未知状态不应是终态")
	}
}
