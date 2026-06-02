CREATE TABLE IF NOT EXISTS process_authorities (
    process_id      TEXT NOT NULL REFERENCES processes(id),
    subject_user_id TEXT NOT NULL REFERENCES users(id),
    created_at      TEXT NOT NULL,
    PRIMARY KEY (process_id, subject_user_id)
);
