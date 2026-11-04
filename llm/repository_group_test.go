package llm

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// setupGroupTestDB は group_id の列を持つ（schema.sql の今の形の）llm_logs を作る。
func setupGroupTestDB(t *testing.T) *LLMRepository {
	t.Helper()
	db := setupPurgeTestDB(t)
	if err := db.Exec("ALTER TABLE system.llm_logs ADD COLUMN group_id TEXT NULL").Error; err != nil {
		t.Fatalf("failed to add group_id: %v", err)
	}
	return NewLLMRepository(db)
}

func TestLLMRepository_SaveLog_GroupID(t *testing.T) {
	ctx := context.Background()
	tests := map[string]struct {
		want    sql.NullString
		groupID string
	}{
		"GroupID を渡せばその値を保存する":  {want: sql.NullString{String: "helpful-c7", Valid: true}, groupID: "helpful-c7"},
		"GroupID が空なら NULL のまま": {want: sql.NullString{}, groupID: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			repo := setupGroupTestDB(t)

			id, err := repo.SaveLog(ctx, &LLMLog{FuncFeature: "f", Status: LogStatusSuccess, GroupID: tc.groupID, CreatedAt: time.Now()})
			if err != nil {
				t.Fatalf("SaveLog: %v", err)
			}

			var got sql.NullString
			if err := repo.db.Raw("SELECT group_id FROM system.llm_logs WHERE id = ?", id).Scan(&got).Error; err != nil {
				t.Fatalf("select: %v", err)
			}
			if got != tc.want {
				t.Errorf("group_id = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// group_id の列を足していない（v0.1.0 の schema.sql で作った）DB でも、GroupID を渡さなければ今までどおり書けて読めること
func TestLLMRepository_group_idの列が無いDBでも動く(t *testing.T) {
	ctx := context.Background()
	repo := NewLLMRepository(setupPurgeTestDB(t))

	if _, err := repo.SaveLog(ctx, &LLMLog{FuncFeature: "f", Status: LogStatusSuccess, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("SaveLog: %v", err)
	}
	count, err := repo.GetDailyUsageCount(ctx, "f")
	if err != nil {
		t.Fatalf("GetDailyUsageCount: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
	var logs []LLMLog
	if err := repo.db.Find(&logs).Error; err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(logs) != 1 || logs[0].GroupID != "" {
		t.Errorf("logs = %+v, want 1 件で GroupID は空", logs)
	}
}

// LLMLog を埋め込んだ呼び出し元の構造体を SaveLog を通さず Create しても、group_id の列が無い DB に書けること
func TestLLMLog_埋め込んだ構造体を直接Createしてもgroup_idの列が無いDBに書ける(t *testing.T) {
	db := setupPurgeTestDB(t)
	if err := db.Exec("ALTER TABLE system.llm_logs ADD COLUMN company_id INTEGER").Error; err != nil {
		t.Fatalf("failed to add company_id: %v", err)
	}
	type logWithCompany struct {
		LLMLog
		CompanyID int64
	}

	row := &logWithCompany{LLMLog: LLMLog{FuncFeature: "f", Status: LogStatusSuccess, CreatedAt: time.Now()}, CompanyID: 7}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if row.ID == 0 {
		t.Errorf("ID が採番されていない")
	}
}
