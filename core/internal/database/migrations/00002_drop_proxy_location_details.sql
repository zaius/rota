-- +goose Up
-- Routing uses only the country; nothing reads the finer location or the ISP.
ALTER TABLE proxies
    DROP COLUMN region_name,
    DROP COLUMN city_name,
    DROP COLUMN latitude,
    DROP COLUMN longitude,
    DROP COLUMN isp;
