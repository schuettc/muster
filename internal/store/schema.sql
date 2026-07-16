CREATE TABLE IF NOT EXISTS agents (
    alias         TEXT PRIMARY KEY,
    role          TEXT NOT NULL DEFAULT '',
    model_type    TEXT NOT NULL DEFAULT '',
    socket_path   TEXT NOT NULL DEFAULT '',
    pane_id       TEXT NOT NULL DEFAULT '',
    session_name  TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT '',
    project       TEXT NOT NULL DEFAULT '',
    label         TEXT NOT NULL DEFAULT '',
    label_manual  INTEGER NOT NULL DEFAULT 0,
    last_read_at  INTEGER NOT NULL DEFAULT 0,
    registered_at INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS threads (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,                 -- 'message' | 'task'
    from_agent TEXT NOT NULL,
    to_kind    TEXT NOT NULL,                 -- 'agent' | 'role' | 'broadcast'
    to_target  TEXT NOT NULL DEFAULT '',
    subject    TEXT NOT NULL DEFAULT '',
    ref        TEXT NOT NULL DEFAULT '',
    status     TEXT,                          -- NULL for messages
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_threads_recipient ON threads(to_kind, to_target);

CREATE TABLE IF NOT EXISTS entries (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id     INTEGER NOT NULL REFERENCES threads(id),
    from_agent    TEXT NOT NULL,
    body          TEXT NOT NULL DEFAULT '',
    status_change TEXT,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entries_thread ON entries(thread_id);

CREATE TABLE IF NOT EXISTS kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        INTEGER NOT NULL,
    kind      TEXT NOT NULL,                 -- send|task|reply|claim|transition|nudge|notify|read
    agent     TEXT NOT NULL DEFAULT '',
    target    TEXT NOT NULL DEFAULT '',      -- 'agent:x' / 'role:r' / 'broadcast' / bare alias (nudge)
    thread_id INTEGER NOT NULL DEFAULT 0,    -- 0 = no thread (e.g. a read)
    count     INTEGER NOT NULL DEFAULT 0,    -- unread count carried by a notify
    detail    TEXT NOT NULL DEFAULT ''       -- 'lit' | 'cleared' | 'skipped: …' | 'error: …'
);
CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent, id);
