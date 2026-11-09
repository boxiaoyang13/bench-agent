package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/secret"
)

const judgePlain = "sk-judge-plaintext-9f3a-ABCD"

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thvv.db")
	st, err := Open(path, time.Second, 4)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// dbContains 检查库文件（含 WAL/SHM）里是否出现明文。
func dbContains(t *testing.T, path, plain string) bool {
	t.Helper()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), plain) {
			return true
		}
	}
	return false
}

func testBox(t *testing.T) *secret.Box {
	t.Helper()
	box, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("初始化加密失败: %v", err)
	}
	return box
}

func newTestRun(id string, judgeKeyEnc []byte) *domain.Run {
	return &domain.Run{
		ID: id, Tool: "thvv", Capability: domain.CapEval, Status: domain.StatusQueued,
		Target: domain.TargetSnapshot{TargetID: "tgt_x", Model: "m", Vendor: "v"},
		Plan: domain.Plan{
			Mode: "single", Datasets: []string{"hle"},
			Judge: &domain.JudgeSpec{
				Model: "glm-5.3", BaseURL: "https://gw.example/v1",
				APIKey: judgePlain, APIKeyHint: secret.Hint(judgePlain),
			},
		},
		JudgeKeyEnc: judgeKeyEnc,
		CreatedAt:   time.Now().UTC(),
		Progress:    domain.Progress{StepsTotal: 1},
	}
}

// TestRunJudgeKeyEncryptedAtRest 落库只存密文；明文既不在 plan_json，也不在响应结构里。
func TestRunJudgeKeyEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	st, path := openTestStore(t)
	box := testBox(t)

	enc, err := box.Encrypt(judgePlain)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	run := newTestRun("run_judgekey_1", enc)
	if err := st.CreateRun(ctx, run, "tester", "idem-1"); err != nil {
		t.Fatalf("写 run 失败: %v", err)
	}

	if dbContains(t, path, judgePlain) {
		t.Fatal("库文件中出现了 judge 明文")
	}

	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("读 run 失败: %v", err)
	}
	if got.Plan.Judge == nil || got.Plan.Judge.APIKey != "" {
		t.Fatalf("读出时不应带明文 judge 凭据: %+v", got.Plan.Judge)
	}
	if got.Plan.Judge.APIKeyHint == "" {
		t.Fatal("应保留 api_key_hint 供展示")
	}
	if plain, derr := box.Decrypt(got.JudgeKeyEnc); derr != nil || plain != judgePlain {
		t.Fatalf("密文应可解回明文: %q err=%v", plain, derr)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "judge_key_enc") {
		t.Fatalf("run JSON 泄漏密文字段: %s", b)
	}

	// 列表接口走的也是同一扫描路径
	list, _, err := st.ListRuns(ctx, RunFilter{Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("列 run 失败: %v n=%d", err, len(list))
	}
	if list[0].Plan.Judge == nil || list[0].Plan.Judge.APIKey != "" {
		t.Fatalf("列表不应带明文 judge 凭据: %+v", list[0].Plan.Judge)
	}
}

// TestScrubLegacyJudgeKeys 历史版本把明文写进 plan_json（并经 SSE 快照落进 run_events），
// 启动清扫后应改为密文列 + 掩码。
func TestScrubLegacyJudgeKeys(t *testing.T) {
	ctx := context.Background()
	st, path := openTestStore(t)
	box := testBox(t)

	legacyPlan := `{"mode":"single","datasets":["hle"],"judge":{"model":"glm-5.3",` +
		`"base_url":"https://gw.example/v1","api_key":"` + judgePlain + `"}}`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO runs
		(run_id,tool,capability,target_id,target_json,plan_json,gate_json,report_json,status,steps_total,steps_done,
		 labels_json,target_vendor,target_model,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"run_legacy_1", "thvv", "eval", "tgt_x", `{"target_id":"tgt_x"}`, legacyPlan,
		"null", "null", "failed", 1, 1, "null", "v", "m", now); err != nil {
		t.Fatalf("插入旧格式 run 失败: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO run_events (run_id,seq,event,data_json,at) VALUES (?,?,?,?,?)`,
		"run_legacy_1", 1, "run.snapshot", `{"judge":{"api_key":"`+judgePlain+`"}}`, now); err != nil {
		t.Fatalf("插入事件失败: %v", err)
	}
	if !dbContains(t, path, judgePlain) {
		t.Fatal("前置条件失败：旧数据里应有明文")
	}

	n, err := st.ScrubLegacyJudgeKeys(ctx, box.Encrypt, secret.Hint)
	if err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应清扫 1 条，实际 %d", n)
	}

	if dbContains(t, path, judgePlain) {
		t.Fatal("清扫后库文件（含 WAL）仍有明文（plan_json 或 run_events）")
	}

	got, err := st.GetRun(ctx, "run_legacy_1")
	if err != nil {
		t.Fatalf("读 run 失败: %v", err)
	}
	if got.Plan.Judge == nil || got.Plan.Judge.APIKeyHint == "" {
		t.Fatalf("清扫后应保留掩码: %+v", got.Plan.Judge)
	}
	if plain, derr := box.Decrypt(got.JudgeKeyEnc); derr != nil || plain != judgePlain {
		t.Fatalf("清扫后密文应可解回明文: %q err=%v", plain, derr)
	}
	if n2, err := st.ScrubLegacyJudgeKeys(ctx, box.Encrypt, secret.Hint); err != nil || n2 != 0 {
		t.Fatalf("二次清扫应无事可做: n=%d err=%v", n2, err)
	}
}

// TestMigrateAddsJudgeKeyColumn 旧库（无该列）打开后应自动补列。
func TestMigrateAddsJudgeKeyColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE runs (run_id TEXT PRIMARY KEY, tool TEXT NOT NULL, capability TEXT NOT NULL,
		target_id TEXT, target_json TEXT NOT NULL, plan_json TEXT NOT NULL, gate_json TEXT, report_json TEXT,
		status TEXT NOT NULL, steps_total INTEGER NOT NULL DEFAULT 0, steps_done INTEGER NOT NULL DEFAULT 0,
		gate_result_json TEXT, summary_json TEXT, error_json TEXT, engine_json TEXT, labels_json TEXT,
		target_vendor TEXT, target_model TEXT, created_by TEXT, idempotency_key TEXT,
		created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT)`); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	raw.Close()

	st, err := Open(path, time.Second, 2)
	if err != nil {
		t.Fatalf("打开旧库失败（补列应成功）: %v", err)
	}
	defer st.Close()

	rows, err := st.db.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		t.Fatalf("读表结构失败: %v", err)
	}
	defer rows.Close()
	has := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫表结构失败: %v", err)
		}
		if name == "judge_key_enc" {
			has = true
		}
	}
	if !has {
		t.Fatal("旧库未补上 judge_key_enc 列")
	}
}
