-- v0.2.0 の DB を v0.3.0 の形にする（llm_logs に created_at だけの index を足す）。
-- LLMRepository.Purge を使うときに流す。期間だけで古いログを探すとき、既存の index は func_feature か group_id が先頭にあって効かない。
-- MySQL 8 では書き込みを止めずに足せるが、行数に比例して時間がかかる。
ALTER TABLE `system`.`llm_logs`
    ADD INDEX `idx_created` (`created_at`), ALGORITHM=INPLACE, LOCK=NONE;
