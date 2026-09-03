PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS entries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    type         TEXT NOT NULL CHECK(type IN ('url', 'note')),
    content      TEXT NOT NULL,
    title        TEXT DEFAULT '',
    summary      TEXT DEFAULT '',
    tags         TEXT DEFAULT '',
    raw_path     TEXT DEFAULT '',
    source       TEXT DEFAULT 'telegram',
    compiled_at  DATETIME,
    embedded_at  DATETIME,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(title, content, summary, tags);

CREATE TRIGGER IF NOT EXISTS entries_fts_insert AFTER INSERT ON entries BEGIN
  INSERT INTO entries_fts(rowid, title, content, summary, tags)
  VALUES (new.id, new.title, new.content, new.summary, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS entries_fts_update AFTER UPDATE ON entries BEGIN
  DELETE FROM entries_fts WHERE rowid=old.id;
  INSERT INTO entries_fts(rowid, title, content, summary, tags)
  VALUES (new.id, new.title, new.content, new.summary, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS entries_fts_delete AFTER DELETE ON entries BEGIN
  DELETE FROM entries_fts WHERE rowid=old.id;
END;
