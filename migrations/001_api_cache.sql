create table if not exists api_cache (
  key text primary key,
  payload jsonb,
  fetched_at timestamptz not null,
  expires_at timestamptz not null,
  last_error text not null default '',
  error_until timestamptz
);
