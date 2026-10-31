package llm

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupPurgeTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}

	// LLMLog.TableName() が system.llm_logs を返すため、スキーマ名でアタッチする
	if err := db.Exec("ATTACH DATABASE ':memory:' AS system").Error; err != nil {
		t.Fatalf("failed to attach system database: %v", err)
	}

	if err := db.Exec(`
		CREATE TABLE system.llm_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			func_feature TEXT,
			memo TEXT,
			status INTEGER,
			input_tokens INTEGER,
			output_tokens INTEGER,
			model_name TEXT,
			input_content TEXT,
			output_content TEXT,
			app_status INTEGER,
			app_error TEXT,
			created_at DATETIME
		)
	`).Error; err != nil {
		t.Fatalf("failed to create table llm_logs: %v", err)
	}

	return db
}

func insertLog(t *testing.T, db *gorm.DB, createdAt time.Time, input, output string) int64 {
	t.Helper()

	log := &LLMLog{
		FuncFeature:   "test-feature",
		Status:        LogStatusSuccess,
		InputTokens:   10,
		OutputTokens:  20,
		ModelName:     "test-model",
		InputContent:  input,
		OutputContent: output,
		CreatedAt:     createdAt,
	}
	if err := db.Create(log).Error; err != nil {
		t.Fatalf("failed to insert llm_log: %v", err)
	}

	return log.ID
}

func TestPurgeOldContent(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	before := now.AddDate(0, 0, -90)

	db := setupPurgeTestDB(t)
	repo := NewLLMRepository(db)

	oldID := insertLog(t, db, now.AddDate(0, 0, -100), "古い入力", "古い出力")
	recentID := insertLog(t, db, now.AddDate(0, 0, -10), "新しい入力", "新しい出力")

	count, err := repo.CountOldContent(ctx, before)
	if err != nil {
		t.Fatalf("CountOldContent failed: %v", err)
	}
	if count != 1 {
		t.Errorf("CountOldContent = %d, want 1", count)
	}

	purged, err := repo.PurgeOldContent(ctx, before)
	if err != nil {
		t.Fatalf("PurgeOldContent failed: %v", err)
	}
	if purged != 1 {
		t.Errorf("PurgeOldContent = %d, want 1", purged)
	}

	// 古い行は本文だけが消え、集計に使うカラムは残る
	var old LLMLog
	if err := db.First(&old, oldID).Error; err != nil {
		t.Fatalf("failed to load old log: %v", err)
	}
	if old.InputContent != "" || old.OutputContent != "" {
		t.Errorf("古いログの本文が残っている: input=%q output=%q", old.InputContent, old.OutputContent)
	}
	if old.InputTokens != 10 || old.OutputTokens != 20 || old.ModelName != "test-model" {
		t.Errorf("古いログの集計用カラムが失われた: %+v", old)
	}

	// 保持期間内の行は触らない
	var recent LLMLog
	if err := db.First(&recent, recentID).Error; err != nil {
		t.Fatalf("failed to load recent log: %v", err)
	}
	if recent.InputContent != "新しい入力" || recent.OutputContent != "新しい出力" {
		t.Errorf("保持期間内のログが消された: %+v", recent)
	}
}

func TestPurgeOldContent_二度目は対象なし(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	before := now.AddDate(0, 0, -90)

	db := setupPurgeTestDB(t)
	repo := NewLLMRepository(db)
	insertLog(t, db, now.AddDate(0, 0, -100), "古い入力", "古い出力")

	if _, err := repo.PurgeOldContent(ctx, before); err != nil {
		t.Fatalf("1回目の PurgeOldContent failed: %v", err)
	}

	purged, err := repo.PurgeOldContent(ctx, before)
	if err != nil {
		t.Fatalf("2回目の PurgeOldContent failed: %v", err)
	}
	if purged != 0 {
		t.Errorf("2回目の PurgeOldContent = %d, want 0", purged)
	}
}

func TestPurgeOldContent_バッチ境界を跨いで全件処理する(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	before := now.AddDate(0, 0, -90)

	db := setupPurgeTestDB(t)
	repo := NewLLMRepository(db)

	const total = purgeBatchSize + 10
	for i := 0; i < total; i++ {
		insertLog(t, db, now.AddDate(0, 0, -100), "古い入力", "古い出力")
	}

	purged, err := repo.PurgeOldContent(ctx, before)
	if err != nil {
		t.Fatalf("PurgeOldContent failed: %v", err)
	}
	if purged != total {
		t.Errorf("PurgeOldContent = %d, want %d", purged, total)
	}
}
