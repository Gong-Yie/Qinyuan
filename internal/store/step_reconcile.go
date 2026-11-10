package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"qinyuan/internal/model"
)

// isTerminalIntentState 意图"这一停就不会自己接着跑"的状态。
//
// paused 故意不在里面：它还会被 resume 续跑，步骤保持 running 才不会诱导 planner
// 另派一条意图去做同一件事；open 同理（放回 frontier 等被重新领取）。
func isTerminalIntentState(s string) bool {
	switch s {
	case model.StateDone, model.StateBlocked, model.StateExhausted, model.StateStopped, model.StateDeleted:
		return true
	}
	return false
}

// reconcileStepsAfterIntent 在意图落终态时，把挂它的 running 步骤推回可执行状态。
//
// 这是计划图的一个空洞：LinkStepIntent 只负责 ready/todo → running，而 running 的
// 唯一出口原本是"事实命中 expected_evidence"或"planner 显式 mark_step_done"。
// 于是意图正常跑完、但事实措辞匹配不上时，步骤永远停在 running —— 计划图显示
// "正在跑"，实际上什么都没在跑。
//
// 规则（纯代码判定，不看模型怎么说）：
//   - 已有 fact/finding 通过 satisfies 连上它 → done；
//   - 还有别的 open/running 意图挂着它 → 保持 running（并行意图还在干）；
//   - 否则 → 依赖都 done 就 ready，否则 todo。
//
// 返回被改过的步骤 id，调用方用同一个 rev 给它们打戳（一次写事件，一个版本号）。
func reconcileStepsAfterIntent(ctx context.Context, tx pgx.Tx, runID, intentID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT st.id,
		       EXISTS (SELECT 1 FROM edges s
		                WHERE s.run_id = $1 AND s.rel = 'satisfies' AND s.dst_id = st.id) AS satisfied,
		       EXISTS (SELECT 1 FROM edges o
		                 JOIN nodes oi ON oi.run_id = o.run_id AND oi.id = o.dst_id
		                WHERE o.run_id = $1 AND o.rel = 'realized_by' AND o.src_id = st.id
		                  AND o.dst_id <> $2 AND oi.kind = 'intent'
		                  AND oi.state IN ('open', 'running')) AS busy,
		       (SELECT count(*) FROM edges d
		                 JOIN nodes dn ON dn.run_id = d.run_id AND dn.id = d.dst_id
		                WHERE d.run_id = $1 AND d.rel = 'depends_on' AND d.src_id = st.id
		                  AND dn.state <> 'done') AS blocked_deps
		  FROM edges e
		  JOIN nodes st ON st.run_id = e.run_id AND st.id = e.src_id
		 WHERE e.run_id = $1 AND e.rel = 'realized_by' AND e.dst_id = $2
		   AND st.kind = 'step' AND st.state = 'running'`, runID, intentID)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id      int64
		done    bool
		busy    bool
		blocked int64
	}
	var cand []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.done, &c.busy, &c.blocked); err != nil {
			rows.Close()
			return nil, err
		}
		cand = append(cand, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var changed []int64
	for _, c := range cand {
		// 还有别的意图在跑同一件事：别抢它的状态。
		if c.busy && !c.done {
			continue
		}
		want := model.StateReady
		switch {
		case c.done:
			want = model.StateDone
		case c.blocked > 0:
			want = model.StateTodo
		}
		tag, err := tx.Exec(ctx, `
			UPDATE nodes
			   SET state = $3, updated_at = now(),
			       completed_at = CASE WHEN $3 = 'done' THEN now() ELSE completed_at END
			 WHERE run_id = $1 AND id = $2 AND kind = 'step' AND state = 'running'`,
			runID, c.id, want)
		if err != nil {
			return changed, err
		}
		if tag.RowsAffected() == 1 {
			changed = append(changed, c.id)
		}
	}
	return changed, nil
}
