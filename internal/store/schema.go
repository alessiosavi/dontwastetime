package store

// migrations are applied in order, each once, each in its own transaction.
// Append only; never edit a shipped migration.
var migrations = []string{
	// 1: base schema.
	`
CREATE TABLE mailbox (
  address          TEXT PRIMARY KEY,
  folders          TEXT NOT NULL DEFAULT 'inbox,sentitems',
  enabled          INTEGER NOT NULL,
  digest           INTEGER NOT NULL,
  poll_interval_s  INTEGER NOT NULL,
  last_ok_at       TEXT,
  state            TEXT NOT NULL DEFAULT 'unknown',
  reason           TEXT NOT NULL DEFAULT '',
  cursor_inbox     TEXT,
  cursor_sentitems TEXT
);
CREATE TABLE message (
  id                  INTEGER PRIMARY KEY,
  mailbox             TEXT NOT NULL,
  graph_id            TEXT NOT NULL,
  folder              TEXT NOT NULL,
  internet_message_id TEXT NOT NULL CHECK (internet_message_id <> ''),
  conversation_id     TEXT NOT NULL DEFAULT '',
  direction           TEXT NOT NULL CHECK (direction IN ('in','out')),
  from_addr           TEXT NOT NULL DEFAULT '',
  from_name           TEXT NOT NULL DEFAULT '',
  to_addrs            TEXT NOT NULL DEFAULT '[]',
  cc_addrs            TEXT NOT NULL DEFAULT '[]',
  subject             TEXT NOT NULL DEFAULT '',
  body_text           TEXT,
  body_preview        TEXT,
  received_at         TEXT NOT NULL,
  sent_at             TEXT,
  has_attachments     INTEGER NOT NULL DEFAULT 0,
  is_auto             INTEGER NOT NULL DEFAULT 0,
  is_bulk             INTEGER NOT NULL DEFAULT 0,
  is_list             INTEGER NOT NULL DEFAULT 0,
  thread_key          TEXT NOT NULL DEFAULT '',
  inserted_at         TEXT NOT NULL,
  body_purged_at      TEXT,
  classified_at       TEXT,
  classify_lease      TEXT,
  UNIQUE (mailbox, internet_message_id)
);
CREATE INDEX message_unclassified ON message(classified_at, received_at);
CREATE INDEX message_thread ON message(thread_key);
CREATE INDEX message_received ON message(received_at);
CREATE TABLE classification (
  message_id       INTEGER PRIMARY KEY REFERENCES message(id),
  label            TEXT NOT NULL,
  entity           TEXT NOT NULL DEFAULT '',
  confidence       REAL NOT NULL,
  urgent           INTEGER NOT NULL DEFAULT 0,
  negative         INTEGER NOT NULL DEFAULT 0,
  method           TEXT NOT NULL CHECK (method IN ('rule','llm','correction')),
  model            TEXT NOT NULL DEFAULT '',
  rationale        TEXT NOT NULL DEFAULT '',
  cost_tokens      INTEGER NOT NULL DEFAULT 0,
  classified_at    TEXT NOT NULL,
  taxonomy_version TEXT NOT NULL DEFAULT ''
);
CREATE INDEX classification_label ON classification(label);
CREATE TABLE thread (
  thread_key              TEXT PRIMARY KEY,
  mailbox                 TEXT NOT NULL,
  subject                 TEXT NOT NULL DEFAULT '',
  last_inbound_at         TEXT,
  last_outbound_at        TEXT,
  last_inbound_from       TEXT NOT NULL DEFAULT '',
  last_inbound_message_id INTEGER,
  needs_reply             INTEGER NOT NULL DEFAULT 0,
  reply_due_at            TEXT,
  priority                TEXT NOT NULL DEFAULT '',
  cassetto                TEXT NOT NULL DEFAULT '',
  muted_at                TEXT,
  muted_by                TEXT NOT NULL DEFAULT '',
  p0_alerted_day          TEXT NOT NULL DEFAULT '',
  updated_at              TEXT
);
CREATE TABLE queue (
  id         INTEGER PRIMARY KEY,
  kind       TEXT NOT NULL,
  ref        INTEGER NOT NULL,
  status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','done','failed')),
  attempts   INTEGER NOT NULL DEFAULT 0,
  not_before TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX queue_claim ON queue(status, not_before);
CREATE TABLE corrections (
  id         INTEGER PRIMARY KEY,
  message_id INTEGER NOT NULL REFERENCES message(id),
  old_label  TEXT NOT NULL,
  new_label  TEXT NOT NULL,
  by         TEXT NOT NULL,
  at         TEXT NOT NULL
);
CREATE TABLE llm_usage (
  day        TEXT PRIMARY KEY,
  calls      INTEGER NOT NULL DEFAULT 0,
  in_tokens  INTEGER NOT NULL DEFAULT 0,
  out_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE digest (
  date        TEXT PRIMARY KEY,
  path        TEXT NOT NULL,
  sent_at     TEXT,
  recipients  TEXT NOT NULL,
  health_json TEXT NOT NULL
);
CREATE TABLE action_log (
  id           INTEGER PRIMARY KEY,
  at           TEXT NOT NULL,
  actor        TEXT NOT NULL,
  action       TEXT NOT NULL,
  target       TEXT NOT NULL,
  details_json TEXT NOT NULL DEFAULT '{}'
);
`, // 2: first_classified_at — when a message first became visible to the
	// digest (a later correction must not re-list it).
	`
ALTER TABLE message ADD COLUMN first_classified_at TEXT;
UPDATE message SET first_classified_at = classified_at WHERE classified_at IS NOT NULL;
`,
	// 3: explicit reply decisions; older classifications retain label fallback.
	`
ALTER TABLE classification ADD COLUMN reply_expected INTEGER;
`,
	// 4: reversible triage, acknowledged by logical inbound rather than mailbox id.
	`
ALTER TABLE thread ADD COLUMN ack_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE thread ADD COLUMN ack_inbound_at TEXT;
ALTER TABLE thread ADD COLUMN ack_inbound_imid TEXT NOT NULL DEFAULT '';
ALTER TABLE thread ADD COLUMN snoozed_until TEXT;
ALTER TABLE thread ADD COLUMN ack_by TEXT NOT NULL DEFAULT '';
ALTER TABLE thread ADD COLUMN ack_at TEXT;
`,
	// 5: sync freshness is independent of access probes and truncated passes.
	`
ALTER TABLE mailbox ADD COLUMN last_sync_completed_at TEXT;
ALTER TABLE mailbox ADD COLUMN sync_truncated INTEGER NOT NULL DEFAULT 0;
`,
}
