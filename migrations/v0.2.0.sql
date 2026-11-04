-- v0.1.0 の schema.sql で作った DB を v0.2.0 の形にする（llm_logs に集計用の group_id を足す）。
-- ChatRequest.GroupID を使うときだけ流せばよい。使わないなら流さなくても v0.2.0 はそのまま動く（GroupID が空なら INSERT に group_id を含めない）。
-- 列は末尾に足す（途中に AFTER で足すと MySQL 8.0.29 より前は表を作り直す）。インデックスの作成は行数に比例して時間がかかる。
ALTER TABLE `system`.`llm_logs`
    ADD COLUMN `group_id` VARCHAR(64) NULL DEFAULT NULL COMMENT '集計用の値（使う側が決める・製品名で始める・未指定は NULL）',
    ADD INDEX `idx_group_created` (`group_id`, `created_at`);
