-- llm-ledger が使う 2 テーブル（MySQL）。
-- モデルの TableName は `system.llm_function_configs` / `system.llm_logs` を返すので、`system` データベースに作る。
CREATE DATABASE IF NOT EXISTS `system`;
USE `system`;

-- 機能（func_feature）ごとの呼び出し設定。行が無い機能は呼ばずに config_missing で断る。
--
-- daily_limit:
--   - 当日（アプリのタイムゾーンの 0 時起点）の status IN (1:success, 2:failed) の行数がこの値に達したら断る（失敗も枠を食う）。
--   - -1 は無制限、0 は 1 回も呼ばない。止めるだけなら is_active = 0 を使う。
--   - 暴走時の安全弁なので、必要な回数の 1.5 倍程度に置く。必要な回数と同じ値にすると、失敗が 1 回出ただけで足りなくなる。
CREATE TABLE IF NOT EXISTS `llm_function_configs` (
    `func_feature` VARCHAR(50) NOT NULL COMMENT '機能ID',
    `daily_limit` INT NOT NULL DEFAULT 10 COMMENT '1日の実行上限回数（-1 は無制限）',
    `max_tokens` INT NOT NULL DEFAULT 3000 COMMENT '最大トークン数',
    `is_active` BOOLEAN NOT NULL DEFAULT TRUE COMMENT '緊急停止用フラグ',
    `description` VARCHAR(255) COMMENT '管理用メモ',
    `updated_at` DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新日時',
    PRIMARY KEY (`func_feature`)
) COMMENT='LLM機能ごとの設定管理';

-- 呼び出し 1 回ごとの記録。断った呼び出し（limited / disabled / config_missing）も残す。
-- group_id は集計用の 1 列で、中身の意味は使う側の製品が system に置く集計用のテーブルで持つ（README の「group_id」）。
-- 🔴 この表には今後カラムを足さない。足したい属性は、その製品の集計用のテーブルのカラムにする。
CREATE TABLE IF NOT EXISTS `llm_logs` (
    `id` BIGINT AUTO_INCREMENT NOT NULL COMMENT 'ログID',
    `func_feature` VARCHAR(50) NOT NULL COMMENT '機能ID',
    `group_id` VARCHAR(64) NULL DEFAULT NULL COMMENT '集計用の値（使う側が決める・製品名で始める・未指定は NULL）',
    `memo` VARCHAR(255) COMMENT '実行目的等のメモ',
    `status` INT NOT NULL COMMENT 'ステータス (1:success, 2:failed, 3:limited, 4:disabled, 5:config_missing)',
    `input_tokens` INT NOT NULL DEFAULT 0 COMMENT '入力トークン数',
    `output_tokens` INT NOT NULL DEFAULT 0 COMMENT '出力トークン数',
    `model_name` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '使用モデル名',
    `input_content` MEDIUMTEXT COMMENT '入力プロンプト',
    `output_content` MEDIUMTEXT COMMENT '出力テキスト',
    `app_status` INT DEFAULT 0 COMMENT 'アプリ評価ステータス (0:未評価, 1:valid, 2:invalid)',
    `app_error` TEXT COMMENT 'アプリ検証エラー内容',
    `created_at` DATETIME DEFAULT CURRENT_TIMESTAMP COMMENT '作成日時',
    PRIMARY KEY (`id`),
    INDEX `idx_func_created` (`func_feature`, `created_at`),
    INDEX `idx_group_created` (`group_id`, `created_at`),
    INDEX `idx_created` (`created_at`)
) COMMENT='LLM実行履歴';
