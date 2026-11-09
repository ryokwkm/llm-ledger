package llm

import (
	"context"
	"slices"
	"testing"
	"time"
)

// insertRuleLog は機能と group_id を指定してログを 1 件入れ、その id を返す。groupID が空なら NULL。
func insertRuleLog(t *testing.T, repo *LLMRepository, createdAt time.Time, feature, groupID string) int64 {
	t.Helper()
	log := &LLMLog{
		FuncFeature:   feature,
		GroupID:       groupID,
		Status:        LogStatusSuccess,
		InputTokens:   10,
		OutputTokens:  20,
		ModelName:     "test-model",
		InputContent:  "入力",
		OutputContent: "出力",
		CreatedAt:     createdAt,
	}
	if err := repo.db.Create(log).Error; err != nil {
		t.Fatalf("failed to insert llm_log: %v", err)
	}
	return log.ID
}

func remainingIDs(t *testing.T, repo *LLMRepository) []int64 {
	t.Helper()
	var ids []int64
	if err := repo.db.Model(&LLMLog{}).Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatalf("failed to pluck ids: %v", err)
	}
	return ids
}

func TestPurge_条件に合う古いログだけをレコードごと消す(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	old := now.AddDate(0, 0, -100)

	tests := map[string]struct {
		wantRemaining []string
		rule          PurgeRule
	}{
		"条件が期間だけなら古いものを全部消す": {
			wantRemaining: []string{"recent"},
			rule:          PurgeRule{},
		},
		"GroupPrefix は group_id の前方一致": {
			wantRemaining: []string{"other", "no-group", "recent"},
			rule:          PurgeRule{GroupPrefix: "app-"},
		},
		"GroupIDs はどれかに一致": {
			wantRemaining: []string{"app-a", "no-group", "recent"},
			rule:          PurgeRule{GroupIDs: []string{"app-c2", "other-c1"}},
		},
		"Features は func_feature のどれかに一致": {
			wantRemaining: []string{"app-b", "other", "recent"},
			rule:          PurgeRule{Features: []string{"app-a"}},
		},
		"GroupIDs と Features を両方渡すと両方に合うものだけ": {
			wantRemaining: []string{"app-b", "other", "no-group", "recent"},
			rule:          PurgeRule{GroupIDs: []string{"app-c1", "app-c2"}, Features: []string{"app-a"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			repo := setupGroupTestDB(t)
			ids := map[int64]string{
				insertRuleLog(t, repo, old, "app-a", "app-c1"):   "app-a",
				insertRuleLog(t, repo, old, "app-b", "app-c2"):   "app-b",
				insertRuleLog(t, repo, old, "other", "other-c1"): "other",
				insertRuleLog(t, repo, old, "app-a", ""):         "no-group",
				insertRuleLog(t, repo, now, "app-a", "app-c1"):   "recent",
			}

			rule := tc.rule
			rule.Before = now.AddDate(0, 0, -90)
			rule.Target = PurgeRecords
			res, err := repo.Purge(ctx, rule)
			if err != nil {
				t.Fatalf("Purge: %v", err)
			}

			var got []string
			for _, id := range remainingIDs(t, repo) {
				got = append(got, ids[id])
			}
			if !slices.Equal(got, tc.wantRemaining) {
				t.Errorf("残ったログ = %v, want %v", got, tc.wantRemaining)
			}
			if want := int64(5 - len(tc.wantRemaining)); res.Affected != want {
				t.Errorf("Affected = %d, want %d", res.Affected, want)
			}
			if res.Reason != PurgeCompleted {
				t.Errorf("Reason = %v, want %v", res.Reason, PurgeCompleted)
			}
		})
	}
}

func TestPurge_本文だけ消すとレコードと集計用のカラムは残る(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := setupGroupTestDB(t)
	targetID := insertRuleLog(t, repo, now.AddDate(0, 0, -100), "app-rerank", "app-c1")
	otherID := insertRuleLog(t, repo, now.AddDate(0, 0, -100), "app-rerank", "app-c2")

	res, err := repo.Purge(ctx, PurgeRule{Before: now.AddDate(0, 0, -90), Target: PurgeContent, GroupIDs: []string{"app-c1"}})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Affected != 1 {
		t.Errorf("Affected = %d, want 1", res.Affected)
	}

	var target, other LLMLog
	if err := repo.db.First(&target, targetID).Error; err != nil {
		t.Fatalf("load target: %v", err)
	}
	if target.InputContent != "" || target.OutputContent != "" {
		t.Errorf("本文が残っている: input=%q output=%q", target.InputContent, target.OutputContent)
	}
	if target.InputTokens != 10 || target.OutputTokens != 20 || target.ModelName != "test-model" {
		t.Errorf("集計用のカラムが失われた: %+v", target)
	}
	if err := repo.db.First(&other, otherID).Error; err != nil {
		t.Fatalf("load other: %v", err)
	}
	if other.InputContent != "入力" {
		t.Errorf("条件に合わないログの本文が消えた: %+v", other)
	}
}

func TestPurge_GroupPrefixの_と百分率記号は文字どおりに比べる(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := setupGroupTestDB(t)
	old := now.AddDate(0, 0, -100)
	insertRuleLog(t, repo, old, "f", "my_app-c1")
	keepUnderscore := insertRuleLog(t, repo, old, "f", "myXapp-c1")
	insertRuleLog(t, repo, old, "f", "50%-c1")
	keepPercent := insertRuleLog(t, repo, old, "f", "50X-c1")

	for _, prefix := range []string{"my_app-", "50%-"} {
		if _, err := repo.Purge(ctx, PurgeRule{Before: now.AddDate(0, 0, -90), Target: PurgeRecords, GroupPrefix: prefix}); err != nil {
			t.Fatalf("Purge(%q): %v", prefix, err)
		}
	}

	if got, want := remainingIDs(t, repo), []int64{keepUnderscore, keepPercent}; !slices.Equal(got, want) {
		t.Errorf("残ったログ = %v, want %v", got, want)
	}
}

func TestPurge_チャンクの境界を跨いで全部消す(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := setupGroupTestDB(t)
	for range 10 {
		insertRuleLog(t, repo, now.AddDate(0, 0, -100), "f", "")
	}

	res, err := repo.Purge(ctx, PurgeRule{Before: now.AddDate(0, 0, -90), Target: PurgeRecords, ChunkSize: 3})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Affected != 10 || res.Reason != PurgeCompleted {
		t.Errorf("res = %+v, want Affected 10・PurgeCompleted", res)
	}
	if got := remainingIDs(t, repo); len(got) != 0 {
		t.Errorf("消し残し = %v", got)
	}
}

func TestPurge_時間帯の外では何もしない(t *testing.T) {
	ctx := context.Background()
	repo := setupGroupTestDB(t)
	noon := time.Date(2026, 11, 10, 12, 0, 0, 0, time.Local)
	repo.now = func() time.Time { return noon }
	insertRuleLog(t, repo, noon.AddDate(0, 0, -100), "f", "")

	res, err := repo.Purge(ctx, PurgeRule{
		Before: noon.AddDate(0, 0, -90),
		Target: PurgeRecords,
		Window: PurgeWindow{From: "02:00", To: "05:00"},
	})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Affected != 0 || res.Reason != PurgeOutsideWindow {
		t.Errorf("res = %+v, want Affected 0・PurgeOutsideWindow", res)
	}
	if got := remainingIDs(t, repo); len(got) != 1 {
		t.Errorf("時間帯の外で消した: 残り %v", got)
	}
}

func TestPurge_時間帯の終わりでチャンクの間に止まる(t *testing.T) {
	ctx := context.Background()
	repo := setupGroupTestDB(t)
	// 時刻を見るたびに 1 分進む。04:58 と 04:59 は時間帯の中、05:00 で外に出る
	clock := time.Date(2026, 11, 10, 4, 58, 0, 0, time.Local)
	repo.now = func() time.Time {
		cur := clock
		clock = clock.Add(time.Minute)
		return cur
	}
	for range 6 {
		insertRuleLog(t, repo, clock.AddDate(0, 0, -100), "f", "")
	}

	res, err := repo.Purge(ctx, PurgeRule{
		Before:    clock.AddDate(0, 0, -90),
		Target:    PurgeRecords,
		ChunkSize: 2,
		Window:    PurgeWindow{From: "02:00", To: "05:00"},
	})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Affected == 0 || res.Affected >= 6 || res.Reason != PurgeWindowEnded {
		t.Errorf("res = %+v, want 一部だけ消して PurgeWindowEnded", res)
	}
	if got := remainingIDs(t, repo); int64(len(got)) != 6-res.Affected {
		t.Errorf("残り %d 件, want %d", len(got), 6-res.Affected)
	}
}

func TestPurgeWindow_日をまたぐ時間帯(t *testing.T) {
	w, err := PurgeWindow{From: "23:00", To: "05:00"}.parse()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tests := map[string]struct {
		want bool
		hm   [2]int
	}{
		"始まりの時刻は中":  {want: true, hm: [2]int{23, 0}},
		"0 時を過ぎても中": {want: true, hm: [2]int{1, 30}},
		"終わりの直前は中":  {want: true, hm: [2]int{4, 59}},
		"終わりの時刻は外":  {want: false, hm: [2]int{5, 0}},
		"昼は外":       {want: false, hm: [2]int{12, 0}},
		"始まりの直前は外":  {want: false, hm: [2]int{22, 59}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			at := time.Date(2026, 11, 10, tc.hm[0], tc.hm[1], 0, 0, time.Local)
			if got := w.contains(at); got != tc.want {
				t.Errorf("contains(%s) = %v, want %v", at.Format("15:04"), got, tc.want)
			}
		})
	}
}

func TestPurge_ルールの誤りはエラーにして何も消さない(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	before := now.AddDate(0, 0, -90)

	tests := map[string]PurgeRule{
		"Before が空":      {Target: PurgeRecords},
		"Target が空":      {Before: before},
		"ChunkSize が負":   {Before: before, Target: PurgeRecords, ChunkSize: -1},
		"時間帯の片方だけ":       {Before: before, Target: PurgeRecords, Window: PurgeWindow{From: "02:00"}},
		"時間帯の形が違う":       {Before: before, Target: PurgeRecords, Window: PurgeWindow{From: "2時", To: "05:00"}},
		"時間帯の始まりと終わりが同じ": {Before: before, Target: PurgeRecords, Window: PurgeWindow{From: "02:00", To: "02:00"}},
	}
	for name, rule := range tests {
		t.Run(name, func(t *testing.T) {
			repo := setupGroupTestDB(t)
			insertRuleLog(t, repo, now.AddDate(0, 0, -100), "f", "a")

			if _, err := repo.Purge(ctx, rule); err == nil {
				t.Error("Purge がエラーを返さなかった")
			}
			if _, err := repo.CountPurgeTargets(ctx, rule); err == nil {
				t.Error("CountPurgeTargets がエラーを返さなかった")
			}
			if got := remainingIDs(t, repo); len(got) != 1 {
				t.Errorf("誤ったルールで消した: 残り %v", got)
			}
		})
	}
}

func TestCountPurgeTargets_Purgeが消す件数と同じ(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	repo := setupGroupTestDB(t)
	insertRuleLog(t, repo, now.AddDate(0, 0, -100), "app-a", "app-c1")
	insertRuleLog(t, repo, now.AddDate(0, 0, -100), "app-a", "app-c1")
	insertRuleLog(t, repo, now.AddDate(0, 0, -100), "other", "other-c1")
	insertRuleLog(t, repo, now, "app-a", "app-c1")
	rule := PurgeRule{Before: now.AddDate(0, 0, -90), Target: PurgeContent, GroupPrefix: "app-"}

	count, err := repo.CountPurgeTargets(ctx, rule)
	if err != nil {
		t.Fatalf("CountPurgeTargets: %v", err)
	}
	res, err := repo.Purge(ctx, rule)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if count != 2 || res.Affected != count {
		t.Errorf("count = %d, Affected = %d, want どちらも 2", count, res.Affected)
	}

	// 本文を消したものは数えない（数え続けると、毎晩同じ件数を報告し続ける）
	after, err := repo.CountPurgeTargets(ctx, rule)
	if err != nil {
		t.Fatalf("CountPurgeTargets: %v", err)
	}
	if after != 0 {
		t.Errorf("消した後の count = %d, want 0", after)
	}
}
