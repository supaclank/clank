-- +goose Up
-- add column "config" to table: "sessions"
ALTER TABLE `sessions` ADD COLUMN `config` text NOT NULL DEFAULT '{}';

-- +goose Down
-- reverse: add column "config" to table: "sessions"
ALTER TABLE `sessions` DROP COLUMN `config`;
