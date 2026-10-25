-- +goose Up
ALTER TABLE sessions ADD COLUMN provision_cancelled BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE sessions DROP COLUMN provision_cancelled;
