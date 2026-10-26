-- +goose Up
-- +goose StatementBegin
-- A coordinator question stays open until it is answered explicitly: Ed answers it
-- (the reply names it) or the coordinator records it answered. How it closed, and
-- the coordinator's note, are kept with the notification.
ALTER TABLE notifications ADD COLUMN resolution TEXT NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN resolution_note TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE notifications DROP COLUMN resolution_note;
ALTER TABLE notifications DROP COLUMN resolution;
-- +goose StatementEnd
