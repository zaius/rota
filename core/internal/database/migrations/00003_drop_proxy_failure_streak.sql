-- +goose Up
-- The rotator keeps the failure streak in memory, and nothing read last_error.
ALTER TABLE proxies
    DROP COLUMN failed_requests,
    DROP COLUMN last_error;
