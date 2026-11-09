package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/apexcloud/thvv/server/internal/domain"
)

// TestJudgeSpecMarshalRedactsAPIKey 契约：judge 凭据永不进 JSON。
func TestJudgeSpecMarshalRedactsAPIKey(t *testing.T) {
	plan := domain.Plan{
		Datasets: []string{"hle"},
		Judge: &domain.JudgeSpec{
			Model: "glm-5.3", BaseURL: "https://gw.example/v1",
			APIKey: "sk-judge-plain-1234", APIKeyHint: "••••••••1234",
		},
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "sk-judge-plain-1234") || strings.Contains(s, `"api_key"`) {
		t.Fatalf("plan JSON 泄漏明文 judge 凭据: %s", s)
	}
	if !strings.Contains(s, "api_key_hint") {
		t.Fatalf("plan JSON 应带 api_key_hint: %s", s)
	}
}

// TestJudgeSpecUnmarshalAcceptsAPIKey 输入侧仍接受 api_key（写入用）。
func TestJudgeSpecUnmarshalAcceptsAPIKey(t *testing.T) {
	var p domain.Plan
	raw := `{"datasets":["hle"],"judge":{"model":"glm-5.3","base_url":"u","api_key":"sk-in-9999"}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Judge == nil || p.Judge.APIKey != "sk-in-9999" {
		t.Fatalf("judge 明文未解析: %+v", p.Judge)
	}
	// 回写（落库前）不再带明文
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "sk-in-9999") {
		t.Fatalf("二次序列化泄漏明文: %s", b)
	}
}

// TestRunMarshalOmitsJudgeKeyEnc 密文列只走 DB，不进 API 响应。
func TestRunMarshalOmitsJudgeKeyEnc(t *testing.T) {
	r := domain.Run{JudgeKeyEnc: []byte{1, 2, 3, 4}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "judge_key_enc") || strings.Contains(string(b), "AQIDBA") {
		t.Fatalf("run JSON 泄漏 judge 密文: %s", b)
	}
}

// TestJudgeSpecHasKey 掩码也算「已配置」，供 probe / 校验使用。
func TestJudgeSpecHasKey(t *testing.T) {
	var nilSpec *domain.JudgeSpec
	if nilSpec.HasKey() {
		t.Fatal("nil 不应算配置了凭据")
	}
	if (&domain.JudgeSpec{APIKeyHint: "••••1234"}).HasKey() != true {
		t.Fatal("有掩码应算已配置")
	}
	if (&domain.JudgeSpec{Model: "x"}).HasKey() {
		t.Fatal("只有 model 不算配置了凭据")
	}
}
