create table if not exists miniblog_posts (
    id bigserial primary key,
    body text not null,
    chat_id bigint not null,
    telegram_message_id bigint,
    created_at timestamptz not null default now()
);

create table if not exists bot_state (
    key text primary key,
    value text not null
);
