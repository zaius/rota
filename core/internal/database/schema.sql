CREATE TABLE settings (
    key VARCHAR(255) PRIMARY KEY,
    value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE proxy_sources (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    url TEXT NOT NULL,
    protocol VARCHAR(20) NOT NULL DEFAULT 'http',
    enabled BOOLEAN NOT NULL DEFAULT true,
    interval_minutes INTEGER NOT NULL DEFAULT 60,
    last_fetched_at TIMESTAMP,
    last_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    last_total INTEGER NOT NULL DEFAULT 0,
    cleanup_enabled BOOLEAN NOT NULL DEFAULT false,
    cleanup_days INTEGER NOT NULL DEFAULT 7,
    format TEXT NOT NULL DEFAULT '[protocol://][user[:pass]@]host:port'
);
CREATE INDEX idx_proxy_sources_enabled ON proxy_sources(enabled);

CREATE TABLE proxies (
    id SERIAL PRIMARY KEY,
    address VARCHAR(255) NOT NULL,
    protocol VARCHAR(20) NOT NULL DEFAULT 'http',
    username VARCHAR(255),
    password TEXT,
    status VARCHAR(20) NOT NULL DEFAULT 'idle',
    requests BIGINT NOT NULL DEFAULT 0,
    successful_requests BIGINT NOT NULL DEFAULT 0,
    failed_requests BIGINT NOT NULL DEFAULT 0,
    avg_response_time INTEGER DEFAULT 0,
    last_check TIMESTAMP,
    last_error TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
    country_code VARCHAR(3),
    country_name VARCHAR(100),
    region_name VARCHAR(100),
    city_name VARCHAR(100),
    latitude DOUBLE PRECISION,
    longitude DOUBLE PRECISION,
    isp VARCHAR(255),
    geo_updated_at TIMESTAMP,
    tags TEXT[] NOT NULL DEFAULT '{}',
    source_id INTEGER REFERENCES proxy_sources(id) ON DELETE CASCADE,
    last_seen_at TIMESTAMP,
    cooldown_until TIMESTAMP,
    cooldown_reason TEXT,
    CONSTRAINT unique_proxy_address_protocol UNIQUE (address, protocol)
);
CREATE INDEX idx_proxies_address ON proxies(address);
CREATE INDEX idx_proxies_status ON proxies(status);
CREATE INDEX idx_proxies_protocol ON proxies(protocol);
CREATE INDEX idx_proxies_country_code ON proxies(country_code);
CREATE INDEX idx_proxies_region_name ON proxies(region_name);
CREATE INDEX idx_proxies_tags ON proxies USING gin(tags);
CREATE INDEX idx_proxies_source_id ON proxies(source_id);
CREATE INDEX idx_proxies_source_last_seen ON proxies(source_id, last_seen_at) WHERE source_id IS NOT NULL;
CREATE INDEX idx_proxies_cooldown_until ON proxies(cooldown_until) WHERE cooldown_until IS NOT NULL;

-- A domain cooldown takes a proxy out of rotation for one target domain and
-- its subdomains; a scope cooldown, for one session scope.
CREATE TABLE proxy_domain_cooldowns (
    proxy_id INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
    domain VARCHAR(255) NOT NULL,
    cooldown_until TIMESTAMPTZ NOT NULL,
    reason TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 4),
    invalid BOOLEAN NOT NULL DEFAULT FALSE,
    recovery_after TIMESTAMPTZ,
    PRIMARY KEY (proxy_id, domain)
);
CREATE INDEX idx_proxy_domain_cooldowns_until ON proxy_domain_cooldowns(cooldown_until);

CREATE TABLE proxy_scope_cooldowns (
    proxy_id INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    cooldown_until TIMESTAMPTZ NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count BETWEEN 0 AND 4),
    invalid BOOLEAN NOT NULL DEFAULT FALSE,
    recovery_after TIMESTAMPTZ,
    PRIMARY KEY (proxy_id, scope)
);
CREATE INDEX idx_proxy_scope_cooldowns_until ON proxy_scope_cooldowns(cooldown_until);

-- Custom line formats used before, offered again in the dashboard.
CREATE TABLE format_history (
    id SERIAL PRIMARY KEY,
    format TEXT NOT NULL UNIQUE,
    use_count INTEGER NOT NULL DEFAULT 1,
    last_used_at TIMESTAMP NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);
