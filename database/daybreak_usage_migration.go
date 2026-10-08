package database

import (
	"context"
	"database/sql"
)

// 将旧日志中的 Daybreak 后缀迁到独立字段，恢复 effective_model 的模型语义。
func (db *DB) backfillDaybreakUsageProgram(ctx context.Context, tx *sql.Tx) error {
	for _, variant := range []struct{ suffix, program string }{
		{"-daybreak-blue", "daybreak_blue"},
		{"-daybreak-red", "daybreak_red"},
	} {
		_, err := tx.ExecContext(ctx, `
			UPDATE usage_logs
			SET daybreak_program = $1,
			    effective_model = SUBSTR(effective_model, 1, LENGTH(effective_model) - LENGTH($2))
			WHERE COALESCE(daybreak_program, '') = ''
			  AND LOWER(effective_model) LIKE $3
		`, variant.program, variant.suffix, "%"+variant.suffix)
		if err != nil {
			return err
		}
	}
	return nil
}
