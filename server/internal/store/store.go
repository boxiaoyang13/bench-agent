// Package store 提供持久化。v1 只实现 SQLite（WAL），接口留了切换点。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"

	_ "modernc.org/sqlite"
)

// ErrNotFound 表示资源不存在。
var ErrNotFound = errors.New("not found")

// ErrConflict 表示唯一约束冲突。
var ErrConflict = errors.New("conflict")

type Store struct {
	db *sql.DB
}

// Open 打开（并初始化）SQLite 库。
func Open(path string, busyTimeout time.Duration, maxConns int) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)",
		path, busyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		db.SetMaxOpenConns(maxConns)
	}
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS targets (
			target_id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			vendor TEXT,
			api_url TEXT NOT NULL,
			protocol TEXT NOT NULL,
			model TEXT NOT NULL,
			api_key_enc BLOB,
			api_key_hint TEXT,
			tokenizer_json TEXT,
			defaults_json TEXT,
			labels_json TEXT,
			health_json TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			deleted_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_targets_deleted ON targets(deleted_at)`,

		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			tool TEXT NOT NULL,
			capability TEXT NOT NULL,
			target_id TEXT,
			target_json TEXT NOT NULL,
			plan_json TEXT NOT NULL,
			gate_json TEXT,
			report_json TEXT,
			status TEXT NOT NULL,
			steps_total INTEGER NOT NULL DEFAULT 0,
			steps_done INTEGER NOT NULL DEFAULT 0,
			gate_result_json TEXT,
			summary_json TEXT,
			error_json TEXT,
			engine_json TEXT,
			labels_json TEXT,
			target_vendor TEXT,
			target_model TEXT,
			created_by TEXT,
			idempotency_key TEXT,
			created_at TEXT NOT NULL,
			started_at TEXT,
			finished_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_created ON runs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_idem ON runs(idempotency_key) WHERE idempotency_key IS NOT NULL`,

		`CREATE TABLE IF NOT EXISTS steps (
			run_id TEXT NOT NULL,
			step_index INTEGER NOT NULL,
			bucket TEXT NOT NULL,
			parallel INTEGER NOT NULL,
			number INTEGER NOT NULL,
			status TEXT NOT NULL,
			success_rate REAL,
			ttft_p50_sec REAL,
			output_tps REAL,
			duration_sec REAL,
			db_path TEXT,
			PRIMARY KEY (run_id, step_index)
		)`,

		`CREATE TABLE IF NOT EXISTS artifacts (
			artifact_id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT,
			size_bytes INTEGER NOT NULL,
			sha256 TEXT,
			path TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_run ON artifacts(run_id)`,

		`CREATE TABLE IF NOT EXISTS run_events (
			run_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			event TEXT NOT NULL,
			data_json TEXT NOT NULL,
			at TEXT NOT NULL,
			PRIMARY KEY (run_id, seq)
		)`,

		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at TEXT NOT NULL,
			actor TEXT,
			action TEXT NOT NULL,
			resource TEXT,
			detail TEXT
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, q)
		}
	}
	return nil
}

// ---------------- Target ----------------

func (s *Store) CreateTarget(ctx context.Context, t *domain.Target) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO targets
		(target_id,name,vendor,api_url,protocol,model,api_key_enc,api_key_hint,tokenizer_json,
		 defaults_json,labels_json,health_json,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Vendor, t.APIURL, string(t.Protocol), t.Model, t.APIKeyEnc, t.APIKeyHint,
		jsonOf(t.Tokenizer), jsonOf(t.Defaults), jsonOf(t.Labels), jsonOf(t.Health),
		ts(t.CreatedAt), ts(t.UpdatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdateTarget(ctx context.Context, t *domain.Target) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET
		name=?, vendor=?, api_url=?, protocol=?, model=?, api_key_enc=?, api_key_hint=?,
		tokenizer_json=?, defaults_json=?, labels_json=?, health_json=?, updated_at=?
		WHERE target_id=? AND deleted_at IS NULL`,
		t.Name, t.Vendor, t.APIURL, string(t.Protocol), t.Model, t.APIKeyEnc, t.APIKeyHint,
		jsonOf(t.Tokenizer), jsonOf(t.Defaults), jsonOf(t.Labels), jsonOf(t.Health), ts(t.UpdatedAt), t.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrConflict
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetTarget(ctx context.Context, id string) (*domain.Target, error) {
	row := s.db.QueryRowContext(ctx, `SELECT target_id,name,vendor,api_url,protocol,model,
		api_key_enc,api_key_hint,tokenizer_json,defaults_json,labels_json,health_json,created_at,updated_at
		FROM targets WHERE target_id=? AND deleted_at IS NULL`, id)
	return scanTarget(row)
}

func (s *Store) GetTargetByName(ctx context.Context, name string) (*domain.Target, error) {
	row := s.db.QueryRowContext(ctx, `SELECT target_id,name,vendor,api_url,protocol,model,
		api_key_enc,api_key_hint,tokenizer_json,defaults_json,labels_json,health_json,created_at,updated_at
		FROM targets WHERE name=? AND deleted_at IS NULL`, name)
	return scanTarget(row)
}

func (s *Store) ListTargets(ctx context.Context, vendor string, limit int) ([]*domain.Target, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT target_id,name,vendor,api_url,protocol,model,api_key_enc,api_key_hint,
		tokenizer_json,defaults_json,labels_json,health_json,created_at,updated_at
		FROM targets WHERE deleted_at IS NULL`
	args := []any{}
	if vendor != "" {
		q += " AND vendor=?"
		args = append(args, vendor)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTarget(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET deleted_at=? WHERE target_id=? AND deleted_at IS NULL`, ts(time.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountTargets(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTarget(sc scanner) (*domain.Target, error) {
	var t domain.Target
	var protocol, createdAt, updatedAt string
	var vendor, hint sql.NullString
	var tokJSON, defJSON, labJSON, healthJSON sql.NullString
	err := sc.Scan(&t.ID, &t.Name, &vendor, &t.APIURL, &protocol, &t.Model,
		&t.APIKeyEnc, &hint, &tokJSON, &defJSON, &labJSON, &healthJSON, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Vendor, t.APIKeyHint = vendor.String, hint.String
	t.Protocol = domain.Protocol(protocol)
	unjsonOf(tokJSON.String, &t.Tokenizer)
	unjsonOf(defJSON.String, &t.Defaults)
	unjsonOf(labJSON.String, &t.Labels)
	unjsonOf(healthJSON.String, &t.Health)
	t.CreatedAt = parseTS(createdAt)
	t.UpdatedAt = parseTS(updatedAt)
	return &t, nil
}

// ---------------- Run ----------------

func (s *Store) CreateRun(ctx context.Context, r *domain.Run, createdBy, idemKey string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs
		(run_id,tool,capability,target_id,target_json,plan_json,gate_json,report_json,status,steps_total,steps_done,
		 labels_json,target_vendor,target_model,created_by,idempotency_key,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Tool, string(r.Capability), r.Target.TargetID, jsonOf(r.Target), jsonOf(r.Plan),
		jsonOf(r.Gate), jsonOf(r.Report), string(r.Status), r.Progress.StepsTotal, r.Progress.StepsDone,
		jsonOf(r.Labels), r.Target.Vendor, r.Target.Model, createdBy, nullIfEmpty(idemKey), ts(r.CreatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrConflict
	}
	return err
}

// RunState 是 runs 表中会被更新的一组字段。
type RunState struct {
	Status     domain.RunStatus
	StepsTotal int
	StepsDone  int
	GateResult *domain.GateResult
	Summary    map[string]any
	Error      *domain.Problem
	Engine     *domain.EngineInfo
	StartedAt  *time.Time
	FinishedAt *time.Time
}

func (s *Store) UpdateRun(ctx context.Context, runID string, st RunState) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET
		status=?, steps_total=?, steps_done=?, gate_result_json=?, summary_json=?, error_json=?,
		engine_json=?, started_at=?, finished_at=?
		WHERE run_id=?`,
		string(st.Status), st.StepsTotal, st.StepsDone, jsonOf(st.GateResult), jsonOf(st.Summary),
		jsonOf(st.Error), jsonOf(st.Engine), tsPtr(st.StartedAt), tsPtr(st.FinishedAt), runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (*domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT run_id,tool,capability,target_json,plan_json,gate_json,report_json,
		status,steps_total,steps_done,gate_result_json,summary_json,error_json,engine_json,labels_json,
		created_at,started_at,finished_at FROM runs WHERE run_id=?`, runID)
	return scanRun(row)
}

func (s *Store) GetRunByIdempotencyKey(ctx context.Context, key string) (*domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT run_id,tool,capability,target_json,plan_json,gate_json,report_json,
		status,steps_total,steps_done,gate_result_json,summary_json,error_json,engine_json,labels_json,
		created_at,started_at,finished_at FROM runs WHERE idempotency_key=?`, key)
	return scanRun(row)
}

type RunFilter struct {
	Statuses   []string
	Capability string
	Tool       string
	TargetID   string
	Vendor     string
	Query      string
	Cursor     string
	Limit      int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*domain.Run, string, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	var where []string
	var args []any
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			ph[i] = "?"
			args = append(args, s)
		}
		where = append(where, "status IN ("+strings.Join(ph, ",")+")")
	}
	if f.Capability != "" {
		where = append(where, "capability=?")
		args = append(args, f.Capability)
	}
	if f.Tool != "" {
		where = append(where, "tool=?")
		args = append(args, f.Tool)
	}
	if f.TargetID != "" {
		where = append(where, "target_id=?")
		args = append(args, f.TargetID)
	}
	if f.Vendor != "" {
		where = append(where, "target_vendor=?")
		args = append(args, f.Vendor)
	}
	if f.Query != "" {
		where = append(where, "(target_model LIKE ? OR target_vendor LIKE ?)")
		args = append(args, "%"+f.Query+"%", "%"+f.Query+"%")
	}
	if f.Cursor != "" {
		// 游标 = 上一页最后一条的 created_at|run_id
		parts := strings.SplitN(f.Cursor, "|", 2)
		if len(parts) == 2 {
			where = append(where, "(created_at < ? OR (created_at = ? AND run_id < ?))")
			args = append(args, parts[0], parts[0], parts[1])
		}
	}
	q := `SELECT run_id,tool,capability,target_json,plan_json,gate_json,report_json,status,steps_total,steps_done,
		gate_result_json,summary_json,error_json,engine_json,labels_json,created_at,started_at,finished_at FROM runs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at DESC, run_id DESC LIMIT ?"
	args = append(args, f.Limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*domain.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		last := out[f.Limit-1]
		next = ts(last.CreatedAt) + "|" + last.ID
		out = out[:f.Limit]
	}
	return out, next, nil
}

func scanRun(sc scanner) (*domain.Run, error) {
	var r domain.Run
	var capability, status, createdAt string
	var targetJSON, planJSON, gateJSON, reportJSON sql.NullString
	var gateResJSON, summaryJSON, errJSON, engineJSON, labelsJSON sql.NullString
	var startedAt, finishedAt sql.NullString
	err := sc.Scan(&r.ID, &r.Tool, &capability, &targetJSON, &planJSON, &gateJSON, &reportJSON, &status,
		&r.Progress.StepsTotal, &r.Progress.StepsDone, &gateResJSON, &summaryJSON, &errJSON,
		&engineJSON, &labelsJSON, &createdAt, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Capability = domain.Capability(capability)
	r.Status = domain.RunStatus(status)
	unjsonOf(targetJSON.String, &r.Target)
	unjsonOf(planJSON.String, &r.Plan)
	unjsonOf(gateJSON.String, &r.Gate)
	unjsonOf(reportJSON.String, &r.Report)
	unjsonOf(gateResJSON.String, &r.GateResult)
	unjsonOf(summaryJSON.String, &r.Summary)
	unjsonOf(errJSON.String, &r.Error)
	unjsonOf(engineJSON.String, &r.Engine)
	unjsonOf(labelsJSON.String, &r.Labels)
	r.CreatedAt = parseTS(createdAt)
	if startedAt.Valid {
		t := parseTS(startedAt.String)
		r.StartedAt = &t
	}
	if finishedAt.Valid {
		t := parseTS(finishedAt.String)
		r.FinishedAt = &t
	}
	r.Progress.Phase = string(r.Status)
	if r.Progress.StepsTotal > 0 {
		r.Progress.Percent = float64(r.Progress.StepsDone) / float64(r.Progress.StepsTotal) * 100
	}
	return &r, nil
}

// ---------------- Steps ----------------

func (s *Store) UpsertStep(ctx context.Context, runID string, st domain.Step) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO steps
		(run_id,step_index,bucket,parallel,number,status,success_rate,ttft_p50_sec,output_tps,duration_sec,db_path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id,step_index) DO UPDATE SET
			status=excluded.status, success_rate=excluded.success_rate,
			ttft_p50_sec=excluded.ttft_p50_sec, output_tps=excluded.output_tps,
			duration_sec=excluded.duration_sec, db_path=excluded.db_path`,
		runID, st.Index, st.Bucket, st.Parallel, st.Number, string(st.Status),
		f64p(st.SuccessRate), f64p(st.TTFTp50Sec), f64p(st.OutputTPS), f64p(st.DurationSec), st.DBPath)
	return err
}

func (s *Store) ListSteps(ctx context.Context, runID string) ([]domain.Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT step_index,bucket,parallel,number,status,
		success_rate,ttft_p50_sec,output_tps,duration_sec,db_path FROM steps WHERE run_id=? ORDER BY step_index`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Step
	for rows.Next() {
		var st domain.Step
		var status string
		var sr, ttft, tps, dur sql.NullFloat64
		var dbp sql.NullString
		if err := rows.Scan(&st.Index, &st.Bucket, &st.Parallel, &st.Number, &status,
			&sr, &ttft, &tps, &dur, &dbp); err != nil {
			return nil, err
		}
		st.Status = domain.StepStatus(status)
		st.SuccessRate = pIf(sr)
		st.TTFTp50Sec = pIf(ttft)
		st.OutputTPS = pIf(tps)
		st.DurationSec = pIf(dur)
		st.DBPath = dbp.String
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------------- Artifacts ----------------

func (s *Store) AddArtifact(ctx context.Context, runID string, a domain.Artifact) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifacts
		(artifact_id,run_id,kind,filename,content_type,size_bytes,sha256,path,created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		a.ID, runID, string(a.Kind), a.Filename, a.ContentType, a.SizeBytes, a.SHA256, a.Path, ts(a.CreatedAt))
	return err
}

func (s *Store) ListArtifacts(ctx context.Context, runID string) ([]domain.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT artifact_id,kind,filename,content_type,size_bytes,sha256,path,created_at
		FROM artifacts WHERE run_id=? ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Artifact
	for rows.Next() {
		var a domain.Artifact
		var kind, createdAt string
		var ctype, sha sql.NullString
		if err := rows.Scan(&a.ID, &kind, &a.Filename, &ctype, &a.SizeBytes, &sha, &a.Path, &createdAt); err != nil {
			return nil, err
		}
		a.ContentType, a.SHA256 = ctype.String, sha.String
		a.Kind = domain.ArtifactKind(kind)
		a.CreatedAt = parseTS(createdAt)
		a.DownloadURL = "/v1/artifacts/" + a.ID
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetArtifact(ctx context.Context, id string) (*domain.Artifact, string, error) {
	var a domain.Artifact
	var runID, kind, createdAt string
	var ctype, sha sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT artifact_id,run_id,kind,filename,content_type,size_bytes,sha256,path,created_at
		FROM artifacts WHERE artifact_id=?`, id).
		Scan(&a.ID, &runID, &kind, &a.Filename, &ctype, &a.SizeBytes, &sha, &a.Path, &createdAt)
	a.ContentType, a.SHA256 = ctype.String, sha.String
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	a.Kind = domain.ArtifactKind(kind)
	a.CreatedAt = parseTS(createdAt)
	a.DownloadURL = "/v1/artifacts/" + a.ID
	return &a, runID, nil
}

// ---------------- Events ----------------

func (s *Store) AppendEvent(ctx context.Context, runID string, seq int, event string, data []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO run_events (run_id,seq,event,data_json,at) VALUES (?,?,?,?,?)`,
		runID, seq, event, string(data), ts(at))
	return err
}

type EventRecord struct {
	Seq   int
	Event string
	Data  []byte
	At    time.Time
}

func (s *Store) EventsSince(ctx context.Context, runID string, seq int, limit int) ([]EventRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,event,data_json,at FROM run_events
		WHERE run_id=? AND seq>? ORDER BY seq LIMIT ?`, runID, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRecord
	for rows.Next() {
		var e EventRecord
		var data, at string
		if err := rows.Scan(&e.Seq, &e.Event, &data, &at); err != nil {
			return nil, err
		}
		e.Data = []byte(data)
		e.At = parseTS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) PruneEvents(ctx context.Context, runID string, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM run_events WHERE run_id=? AND seq <= (
		SELECT MAX(seq) FROM run_events WHERE run_id=?) - ?`, runID, runID, keep)
	return err
}

// ---------------- audit ----------------

func (s *Store) Audit(ctx context.Context, actor, action, resource, detail string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_log (at,actor,action,resource,detail) VALUES (?,?,?,?,?)`,
		ts(time.Now()), actor, action, resource, detail)
}

// ---------------- helpers ----------------

func jsonOf(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	return string(b)
}

func unjsonOf(s string, dst any) {
	if s == "" || s == "null" {
		return
	}
	_ = json.Unmarshal([]byte(s), dst)
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func f64p(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func pIf(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}
