// Read-only access to the desktop's native goal storage (goals_1.sqlite),
// mirroring bridge/goal.py's read_native_goal: cheap enough for every view
// read, with short retry loops so one locked write transaction never makes
// the phone fall back to a stale snapshot.
package goal

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	_ "modernc.org/sqlite"
)

var statusAliases = map[string]string{
	"usage_limited": "usageLimited", "budget_limited": "budgetLimited",
}

// ReadNativeGoal returns the authoritative native goal row for one local
// thread, normalized to the panel shape (camelCase keys, status aliases), or
// nil when the thread has no goal (or the database is missing/locked).
func ReadNativeGoal(home, threadID string) *jsoncompat.Object {
	path := filepath.Join(home, "goals_1.sqlite")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(50)")
	if err != nil {
		return nil
	}
	defer db.Close()
	var objective, status, goalID sql.NullString
	var tokenBudget, tokensUsed, timeUsed, createdAt, updatedAt sql.NullInt64
	const query = `SELECT objective, status, goal_id, token_budget, tokens_used,
		time_used_seconds, created_at_ms, updated_at_ms
		FROM thread_goals WHERE thread_id = ?`
	var scanErr error
	for attempt := 0; attempt < 5; attempt++ {
		scanErr = db.QueryRow(query, threadID).Scan(
			&objective, &status, &goalID, &tokenBudget, &tokensUsed,
			&timeUsed, &createdAt, &updatedAt)
		if scanErr == nil {
			break
		}
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil // authoritative absence
		}
		// The desktop may hold a short write transaction; retry like Python.
		time.Sleep(20 * time.Millisecond)
	}
	if scanErr != nil {
		return nil
	}
	result := jsoncompat.NewObject()
	result.Set("objective", objective.String)
	if aliased, exists := statusAliases[status.String]; exists {
		result.Set("status", aliased)
	} else {
		result.Set("status", status.String)
	}
	result.Set("threadId", threadID)
	result.Set("goalId", goalID.String)
	if tokenBudget.Valid {
		result.Set("tokenBudget", jsoncompat.Num(int(tokenBudget.Int64)))
	}
	if tokensUsed.Valid {
		result.Set("tokensUsed", jsoncompat.Num(int(tokensUsed.Int64)))
	}
	if timeUsed.Valid {
		result.Set("timeUsedSeconds", jsoncompat.Num(int(timeUsed.Int64)))
	}
	if createdAt.Valid {
		result.Set("createdAtMs", jsoncompat.Num(int(createdAt.Int64)))
	}
	if updatedAt.Valid {
		result.Set("updatedAtMs", jsoncompat.Num(int(updatedAt.Int64)))
	}
	if objective.String == "" || status.String == "" {
		return nil
	}
	return result
}

// IdentityOf mirrors goal_identity: the stable fields two goal snapshots
// must share to be "the same goal" (goalId when present, else the
// creation/objective pair).
func IdentityOf(goal *jsoncompat.Object) *jsoncompat.Object {
	identity := jsoncompat.NewObject()
	if goal == nil {
		return identity
	}
	if id, _ := goal.MustGet("goalId").(string); id != "" {
		identity.Set("goalId", id)
		return identity
	}
	if created := goal.MustGet("createdAtMs"); created != nil {
		identity.Set("createdAtMs", created)
	}
	if objective, _ := goal.MustGet("objective").(string); objective != "" {
		identity.Set("objective", objective)
	}
	return identity
}

// SameGoal compares two goal snapshots by identity.
func SameGoal(a, b *jsoncompat.Object) bool {
	return string(jsoncompat.EncodeCompact(IdentityOf(a))) == string(jsoncompat.EncodeCompact(IdentityOf(b)))
}

// Absent proves (via native storage) that a thread has no goal, separating
// "no goal" from "unreadable" for stale-snapshot suppression.
func Absent(home, threadID string) bool {
	path := filepath.Join(home, "goals_1.sqlite")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return false
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var one int
	if err := db.QueryRow(`SELECT 1 FROM thread_goals WHERE thread_id = ? LIMIT 1`, threadID).Scan(&one); err != nil {
		return errors.Is(err, sql.ErrNoRows)
	}
	return false
}
