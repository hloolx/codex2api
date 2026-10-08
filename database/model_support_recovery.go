package database

import (
	"context"
	"time"
)

// ClearUnsupportedModelSince fences recovery against later upstream failures.
func (db *DB) ClearUnsupportedModelSince(ctx context.Context, accountID int64, model string, startedAt, previousReset time.Time) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `DELETE FROM account_model_cooldowns
		WHERE account_id=$1 AND model=$2 AND reason='model_not_supported'
		AND updated_at<=$3 AND reset_at<=$4`, accountID, model, db.timeArg(startedAt), db.timeArg(previousReset))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
