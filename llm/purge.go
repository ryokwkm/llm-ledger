package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// PurgeTarget は Purge の消し方。
type PurgeTarget int

const (
	// PurgeRecords はレコードごと DELETE する。
	PurgeRecords PurgeTarget = iota + 1
	// PurgeContent は入出力の本文 2 カラムだけを NULL にする。
	// トークン数・モデル名・status はコストの集計と RPD の消費実績に使うので残す。
	PurgeContent
)

// PurgeRule は Purge で消すログの条件・消し方・区切り・動いてよい時間帯。条件は全部 AND でつなぐ。
// いつ・どの条件で消すかはサービスごとに違うので、使う側が決めて渡す。
type PurgeRule struct {
	Before      time.Time   // 必須。created_at がこれより前のログを消す
	Target      PurgeTarget // 必須
	GroupIDs    []string    // 任意。group_id がどれかに一致するものだけ
	GroupPrefix string      // 任意。group_id の前方一致（% と _ も文字どおりに比べる）
	Features    []string    // 任意。func_feature がどれかに一致するものだけ
	ChunkSize   int         // 任意。1 回の DELETE・UPDATE で触る件数。0 なら 500
	Window      PurgeWindow // 任意。ゼロ値なら時刻で止めない
}

// PurgeWindow は Purge が動いてよい時間帯。From・To は "15:04" の形で、From が To より後なら日をまたぐ（例 23:00〜05:00）。
// 使う側のプロセスのタイムゾーン（TZ）で比べる（daily_limit の 0 時と同じ）。
type PurgeWindow struct {
	From string
	To   string
}

// PurgeStopReason は Purge が止まった理由。
type PurgeStopReason string

const (
	// PurgeCompleted は条件に合うものを消し切った。
	PurgeCompleted PurgeStopReason = "completed"
	// PurgeOutsideWindow は呼ばれた時刻が時間帯の外で、何もしなかった。
	PurgeOutsideWindow PurgeStopReason = "outside_window"
	// PurgeWindowEnded は途中で時間帯の終わりが来たので止めた。残りは次に呼んだときに消える。
	PurgeWindowEnded PurgeStopReason = "window_ended"
)

// PurgeResult は Purge の結果。
type PurgeResult struct {
	Affected int64 // 消した（PurgeContent なら本文を NULL にした）件数
	Reason   PurgeStopReason
}

// defaultPurgeChunkSize は ChunkSize を省いたときの 1 回の件数。
// mediumtext 2 本を含むため、一括で流すと長時間トランザクションになる。
const defaultPurgeChunkSize = 500

// Purge は rule に合うログをチャンクに分けて消す。チャンクの間で時間帯を確かめ、外に出たらそこで止める。
// 止めても次に呼べば残りが条件に当たるので、どこまで消したかは保存しない。
func (r *LLMRepository) Purge(ctx context.Context, rule PurgeRule) (PurgeResult, error) {
	window, err := rule.validate()
	if err != nil {
		return PurgeResult{}, err
	}
	if !window.contains(r.now()) {
		return PurgeResult{Reason: PurgeOutsideWindow}, nil
	}

	maxID, found, err := r.lastIDBefore(ctx, rule.Before)
	if err != nil || !found {
		return PurgeResult{Reason: PurgeCompleted}, err
	}

	chunkSize := rule.ChunkSize
	if chunkSize == 0 {
		chunkSize = defaultPurgeChunkSize
	}

	var (
		res    PurgeResult
		lastID int64
	)
	// id カーソルで前進させる。述語だけで回すと、削除や更新が空振りしたときに同じ行を無限に読み直す
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !window.contains(r.now()) {
			res.Reason = PurgeWindowEnded
			return res, nil
		}

		var ids []int64
		if err := r.db.WithContext(ctx).Model(&LLMLog{}).
			Scopes(purgeScope(rule)).
			Where("id > ? AND id <= ?", lastID, maxID).
			Order("id").
			Limit(chunkSize).
			Pluck("id", &ids).Error; err != nil {
			return res, err
		}
		if len(ids) == 0 {
			res.Reason = PurgeCompleted
			return res, nil
		}
		lastID = ids[len(ids)-1]

		n, err := r.purgeChunk(ctx, rule.Target, ids)
		res.Affected += n
		if err != nil {
			return res, err
		}
	}
}

// CountPurgeTargets は rule に合い、Purge が消すログの件数を返す（時間帯と ChunkSize は見ない）。消す前の見積もりに使う。
func (r *LLMRepository) CountPurgeTargets(ctx context.Context, rule PurgeRule) (int64, error) {
	if _, err := rule.validate(); err != nil {
		return 0, err
	}
	var count int64
	err := r.db.WithContext(ctx).Model(&LLMLog{}).
		Scopes(purgeScope(rule)).
		Count(&count).Error
	return count, err
}

// CountOldContent は before より古く、まだ本文が残っているログの件数を返す。
func (r *LLMRepository) CountOldContent(ctx context.Context, before time.Time) (int64, error) {
	return r.CountPurgeTargets(ctx, PurgeRule{Before: before, Target: PurgeContent})
}

// PurgeOldContent は before より古いログの入出力本文だけを NULL にし、更新した件数を返す。行は消さない。
// 時間帯や条件を指定するなら Purge を使う。
func (r *LLMRepository) PurgeOldContent(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.Purge(ctx, PurgeRule{Before: before, Target: PurgeContent})
	return res.Affected, err
}

// validate は rule の誤りを確かめ、解釈した時間帯を返す（時間帯の指定が無ければ nil）。
func (rule PurgeRule) validate() (*purgeWindow, error) {
	switch {
	case rule.Before.IsZero():
		return nil, errors.New("PurgeRule.Before が空です")
	case rule.Target != PurgeRecords && rule.Target != PurgeContent:
		return nil, fmt.Errorf("PurgeRule.Target が不正です: %d", rule.Target)
	case rule.ChunkSize < 0:
		return nil, fmt.Errorf("PurgeRule.ChunkSize が負です: %d", rule.ChunkSize)
	}
	return rule.Window.parse()
}

// purgeWindow は PurgeWindow を 0 時からの分に直したもの。
type purgeWindow struct {
	from int
	to   int
}

func (w PurgeWindow) parse() (*purgeWindow, error) {
	if w.From == "" && w.To == "" {
		return nil, nil
	}
	from, err := minuteOfDay(w.From)
	if err != nil {
		return nil, fmt.Errorf("PurgeWindow.From を 15:04 の形で読めません: %w", err)
	}
	to, err := minuteOfDay(w.To)
	if err != nil {
		return nil, fmt.Errorf("PurgeWindow.To を 15:04 の形で読めません: %w", err)
	}
	if from == to {
		return nil, fmt.Errorf("PurgeWindow の From と To が同じです: %s", w.From)
	}
	return &purgeWindow{from: from, to: to}, nil
}

func minuteOfDay(hm string) (int, error) {
	t, err := time.Parse("15:04", hm)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// contains は t が時間帯の中かを返す。時間帯の指定が無ければ（nil）いつでも中。
func (w *purgeWindow) contains(t time.Time) bool {
	if w == nil {
		return true
	}
	t = t.Local()
	m := t.Hour()*60 + t.Minute()
	if w.from < w.to {
		return w.from <= m && m < w.to
	}
	return m >= w.from || m < w.to
}

// lastIDBefore は created_at が before より前のログのうち、最も新しいものの id を返す。
// Purge はこの id までに絞って走査する。絞らないと、最後の空振りの検索が保持期間内の全部のレコードを読む。
// created_at の index（v0.3.0）を逆順にたどる 1 件の検索で済ませる。時計のずれで id と created_at の順が前後した数件は、次に呼んだときに消える。
func (r *LLMRepository) lastIDBefore(ctx context.Context, before time.Time) (int64, bool, error) {
	var ids []int64
	if err := r.db.WithContext(ctx).Model(&LLMLog{}).
		Where("created_at < ?", before).
		Order("created_at DESC, id DESC").
		Limit(1).
		Pluck("id", &ids).Error; err != nil || len(ids) == 0 {
		return 0, false, err
	}
	return ids[0], true, nil
}

// purgeScope は rule に合うログに絞る。Purge と CountPurgeTargets で共有する
// （ずれると、本文を消したログまで数えて見積もりが毎晩同じ件数を報告し続ける）。
// group_id を条件にしないときは group_id のカラムに触れない（v0.1.0 の schema.sql の DB には無い）。
func purgeScope(rule PurgeRule) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		db = db.Where("created_at < ?", rule.Before)
		if len(rule.GroupIDs) > 0 {
			db = db.Where("group_id IN ?", rule.GroupIDs)
		}
		if rule.GroupPrefix != "" {
			db = db.Where("group_id LIKE ? ESCAPE '!'", likeEscaper.Replace(rule.GroupPrefix)+"%")
		}
		if len(rule.Features) > 0 {
			db = db.Where("func_feature IN ?", rule.Features)
		}
		if rule.Target == PurgeContent {
			db = db.Where("(input_content IS NOT NULL OR output_content IS NOT NULL)")
		}
		return db
	}
}

// likeEscaper は LIKE の % と _ を文字どおりに比べるためのエスケープ。
// エスケープ文字を ! にするのは、\ だと MySQL の文字列リテラルでは '\\' と書く必要があり、SQLite では 2 文字になって ESCAPE に渡せないため。
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// purgeChunk は ids のログを target の消し方で消し、消した件数を返す。
func (r *LLMRepository) purgeChunk(ctx context.Context, target PurgeTarget, ids []int64) (int64, error) {
	db := r.db.WithContext(ctx).Model(&LLMLog{}).Where("id IN ?", ids)
	var res *gorm.DB
	if target == PurgeRecords {
		res = db.Delete(&LLMLog{})
	} else {
		res = db.Updates(map[string]any{"input_content": nil, "output_content": nil})
	}
	return res.RowsAffected, res.Error
}
