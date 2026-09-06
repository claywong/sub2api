-- Global IP allowlist is stored in the existing settings table as JSON.
INSERT INTO settings (key, value)
VALUES ('security.ip_allowlist', '["114.55.124.150/32","121.40.190.169/32"]')
ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value)
VALUES ('security.ip_allowlist_enabled', 'true')
ON CONFLICT (key) DO NOTHING;
