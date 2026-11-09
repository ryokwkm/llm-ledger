package llm

import (
	"context"
	"time"

	"gorm.io/gorm"
)

type Repository interface {
	GetConfig(ctx context.Context, feature string) (*LLMFunctionConfig, error)
	GetDailyUsageCount(ctx context.Context, feature string) (int, error)
	SaveLog(ctx context.Context, log *LLMLog) (int64, error)
	UpdateLogValidation(ctx context.Context, logID int64, appStatus AppStatus, appError string) error
}

type LLMRepository struct {
	db  *gorm.DB
	now func() time.Time // Purge が時間帯を確かめる時刻。テストで差し替える
}

func NewLLMRepository(db *gorm.DB) *LLMRepository {
	return &LLMRepository{db: db, now: time.Now}
}

func (r *LLMRepository) GetConfig(ctx context.Context, feature string) (*LLMFunctionConfig, error) {
	var model LLMFunctionConfig
	err := r.db.WithContext(ctx).Where("func_feature = ?", feature).First(&model).Error
	if err != nil {
		return nil, err
	}

	return &model, nil
}

func (r *LLMRepository) GetDailyUsageCount(ctx context.Context, feature string) (int, error) {
	var count int64
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	err := r.db.WithContext(ctx).Model(&LLMLog{}).
		Where("func_feature = ?", feature).
		Where("created_at >= ?", startOfDay).
		Where("status IN ?", []LogStatus{LogStatusSuccess, LogStatusFailed}).
		Count(&count).Error

	return int(count), err
}

// GetModelUsageSince は since 以降の Gemini 呼び出し回数をモデル別に集計する（UsageRepository の実装）。
// 日次クォータ（RPD）の消費実績を復元するためのもので、since には QuotaDayStart（PT 深夜0時）を渡す。
//
// featurePrefix は func_feature の前方一致で、API キーを分けているドメイン単位に絞るために使う
// （空文字なら絞り込まない）。カウント対象は実際に Gemini へ投げた success / failed のみ。
// limited は guard_client が送信前に遮断した記録なので枠を消費していない。
func (r *LLMRepository) GetModelUsageSince(ctx context.Context, since time.Time, featurePrefix string) (map[string]int, error) {
	type row struct {
		ModelName string
		Count     int
	}

	query := r.db.WithContext(ctx).Model(&LLMLog{}).
		Select("model_name, COUNT(*) AS count").
		Where("created_at >= ?", since).
		Where("status IN ?", []LogStatus{LogStatusSuccess, LogStatusFailed}).
		Group("model_name")
	if featurePrefix != "" {
		query = query.Where("func_feature LIKE ?", featurePrefix+"%")
	}

	var rows []row
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}

	usage := make(map[string]int, len(rows))
	for _, rw := range rows {
		if rw.ModelName == "" {
			continue
		}
		usage[rw.ModelName] = rw.Count
	}
	return usage, nil
}

func (r *LLMRepository) SaveLog(ctx context.Context, log *LLMLog) (int64, error) {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	err := r.db.WithContext(ctx).Create(log).Error
	return log.ID, err
}

func (r *LLMRepository) UpdateLogValidation(ctx context.Context, logID int64, appStatus AppStatus, appError string) error {
	return r.db.WithContext(ctx).Model(&LLMLog{}).
		Where("id = ?", logID).
		Updates(map[string]interface{}{
			"app_status": appStatus,
			"app_error":  appError,
		}).Error
}
