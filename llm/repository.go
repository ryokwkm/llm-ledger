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
	db *gorm.DB
}

func NewLLMRepository(db *gorm.DB) *LLMRepository {
	return &LLMRepository{db: db}
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

// purgeBatchSize は PurgeOldContent が 1 回の UPDATE で触る行数。
// mediumtext 2 本を含むため、一括で流すと長時間トランザクションになる。
const purgeBatchSize = 500

// CountOldContent は before より古く、まだ本文が残っているログの件数を返す。
func (r *LLMRepository) CountOldContent(ctx context.Context, before time.Time) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&LLMLog{}).
		Scopes(oldContent(before)).
		Count(&count).Error
	return count, err
}

// PurgeOldContent は before より古いログの入出力本文だけを NULL にし、更新した行数を返す。
// 行は消さない。トークン数・モデル名・status はコスト集計と RPD の消費実績に使うため残す。
func (r *LLMRepository) PurgeOldContent(ctx context.Context, before time.Time) (int64, error) {
	var (
		total  int64
		lastID int64
	)

	// id カーソルで前進させる。述語だけで回すと、更新が空振りしたときに同じ行を無限に読み直す
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}

		var ids []int64
		if err := r.db.WithContext(ctx).Model(&LLMLog{}).
			Scopes(oldContent(before)).
			Where("id > ?", lastID).
			Order("id").
			Limit(purgeBatchSize).
			Pluck("id", &ids).Error; err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		lastID = ids[len(ids)-1]

		res := r.db.WithContext(ctx).Model(&LLMLog{}).
			Where("id IN ?", ids).
			Updates(map[string]any{"input_content": nil, "output_content": nil})
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
}

// oldContent は before より古く、入出力の本文がまだ残っているログに絞る。
// CountOldContent と PurgeOldContent で共有する（ずれると、NULL 化済みの行まで数えて
// dry-run が毎日同じ件数を報告し続ける）。
func oldContent(before time.Time) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where("created_at < ?", before).
			Where("input_content IS NOT NULL OR output_content IS NOT NULL")
	}
}
