package runmgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/apexcloud/thvv/server/internal/domain"
	"github.com/apexcloud/thvv/server/internal/store"
)

// ErrRunActive 表示 run 还在排队/运行中，不能删除。
var ErrRunActive = errors.New("run 正在排队或运行中")

// PurgeFilter 是批量清理的条件（全部为「且」关系）。
// 不传任何条件时只允许 statuses 非空，避免一把清空整个库。
type PurgeFilter struct {
	RunIDs     []string // 显式指定要删的 run（勾选删除）；给出则忽略 Statuses/Capability/Before
	Statuses   []string // 目标状态（只能包含终态）
	Capability string   // 可选：perf | eval | cache
	Before     *time.Time
	DryRun     bool
}

// PurgeResult 是批量清理的结果。
type PurgeResult struct {
	Deleted int      `json:"deleted"`
	DryRun  bool     `json:"dry_run"`
	RunIDs  []string `json:"run_ids"`
	Skipped int      `json:"skipped_active"` // 命中但仍在跑，跳过
}

// Delete 硬删除一条 run：状态检查 → 移除活跃表 → 删库 → 删 runs/<id>/ 目录。
func (m *Manager) Delete(ctx context.Context, runID string) error {
	run, err := m.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if !run.Status.Terminal() {
		return fmt.Errorf("%w（当前 %s，请先取消并等待进入终态）", ErrRunActive, run.Status)
	}
	// 活跃表里可能还留着终态 run（被 worker 跳过的情况），一并摘掉
	m.mu.Lock()
	delete(m.canc, runID)
	m.mu.Unlock()
	if lr := m.liveRun(runID); lr != nil {
		lr.mu.Lock()
		started := lr.started
		lr.mu.Unlock()
		if !started.IsZero() {
			// 已经跑过（终态），可以安全移除
			m.mu.Lock()
			delete(m.live, runID)
			m.mu.Unlock()
		}
	}
	if err := m.st.DeleteRun(ctx, runID); err != nil {
		return err
	}
	m.removeRunDir(runID)
	m.log.Info("run deleted", "run_id", runID, "status", run.Status, "capability", run.Capability)
	return nil
}

// removeRunDir 删除 runs/<id>（含 artifacts / logs / results）。失败只记日志，不回滚库。
func (m *Manager) removeRunDir(runID string) {
	dir := filepath.Join(string(m.cfg.Paths.RunsDir), runID)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		m.log.Warn("删除 run 目录失败", "run_id", runID, "dir", dir, "err", err)
	}
}

// Purge 按条件批量删除终态 run。statuses 为空时默认只清 canceled/failed。
// 传入 RunIDs（勾选删除）时按 id 逐个删，不存在的 id 忽略、运行中的计数到 skipped_active。
func (m *Manager) Purge(ctx context.Context, f PurgeFilter) (PurgeResult, error) {
	res := PurgeResult{DryRun: f.DryRun}
	if len(f.RunIDs) > 0 {
		for _, id := range f.RunIDs {
			r, err := m.st.GetRun(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return res, err
			}
			if !r.Status.Terminal() {
				res.Skipped++
				continue
			}
			res.RunIDs = append(res.RunIDs, id)
			if f.DryRun {
				continue
			}
			if err := m.Delete(ctx, id); err != nil {
				return res, err
			}
			res.Deleted++
		}
		return res, nil
	}
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = []string{string(domain.StatusCanceled), string(domain.StatusFailed)}
	}
	for _, s := range statuses {
		st := domain.RunStatus(s)
		if !st.Terminal() {
			return res, invalid("status %q 不是终态，不能批量删除（运行中请先取消）", s)
		}
	}

	// 分页扫全部命中的 run（上限 100/页）
	var cursor string
	for {
		runs, next, err := m.st.ListRuns(ctx, store.RunFilter{
			Statuses: statuses, Capability: f.Capability, Cursor: cursor, Limit: 100,
		})
		if err != nil {
			return res, err
		}
		for _, r := range runs {
			if f.Before != nil && !r.CreatedAt.Before(*f.Before) {
				continue
			}
			if !r.Status.Terminal() {
				res.Skipped++
				continue
			}
			res.RunIDs = append(res.RunIDs, r.ID)
			if f.DryRun {
				continue
			}
			if err := m.Delete(ctx, r.ID); err != nil {
				return res, err
			}
			res.Deleted++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if f.DryRun {
		res.Deleted = 0
	}
	return res, nil
}
