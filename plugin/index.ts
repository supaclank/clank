import type { Plugin } from "@opencode-ai/plugin";
import { Database } from "bun:sqlite";
import { homedir } from "node:os";
import { join } from "node:path";

const CLANK_DB_PATH = join(homedir(), ".clank", "clank.db");

function openClankDB(): Database {
  const db = new Database(CLANK_DB_PATH, { create: false });
  db.exec("PRAGMA journal_mode=WAL");
  db.exec("PRAGMA busy_timeout=5000");
  // Ensure the session_status table exists (idempotent).
  db.exec(`
    CREATE TABLE IF NOT EXISTS session_status (
      session_id  TEXT PRIMARY KEY,
      status      TEXT NOT NULL DEFAULT 'idle',
      source      TEXT NOT NULL DEFAULT 'opencode',
      unread      INTEGER NOT NULL DEFAULT 1,
      updated_at  INTEGER NOT NULL
    )
  `);
  // Migration for existing tables missing the unread column.
  try {
    db.exec(
      "ALTER TABLE session_status ADD COLUMN unread INTEGER NOT NULL DEFAULT 1"
    );
  } catch {
    // Column already exists — ignore.
  }
  db.exec(
    "CREATE INDEX IF NOT EXISTS idx_session_status_status ON session_status(status)"
  );
  return db;
}

function upsertStatus(
  db: Database,
  sessionID: string,
  status: string,
  unread: number,
  source: string = "opencode"
) {
  db.run(
    `INSERT INTO session_status (session_id, status, source, unread, updated_at)
     VALUES (?, ?, ?, ?, ?)
     ON CONFLICT(session_id) DO UPDATE SET
       status=excluded.status, source=excluded.source,
       unread=excluded.unread, updated_at=excluded.updated_at`,
    [sessionID, status, source, unread, Date.now()]
  );
}

const ClankSessionTracker: Plugin = async () => {
  let db: Database | null = null;

  function getDB(): Database {
    if (!db) {
      db = openClankDB();
    }
    return db;
  }

  return {
    event: async ({ event }) => {
      try {
        // Agent started working — user just sent a message, so not unread.
        if (
          event.type === "session.status" &&
          event.properties.status.type === "busy"
        ) {
          upsertStatus(getDB(), event.properties.sessionID, "busy", 0);
        }

        // Agent finished — session went idle, mark as unread.
        if (event.type === "session.idle") {
          const sessionID = event.properties.sessionID;
          // Only transition to idle if the session hasn't been manually
          // moved to a user-managed state (approved, archived, followup).
          const row = getDB()
            .query(
              "SELECT status FROM session_status WHERE session_id = ?"
            )
            .get(sessionID) as { status: string } | null;
          const userStates = ["approved", "archived", "followup"];
          if (!row || !userStates.includes(row.status)) {
            upsertStatus(getDB(), sessionID, "idle", 1);
          }
        }

        // Session errored — also unread (user should see the error).
        if (event.type === "session.error") {
          const sessionID = event.properties.sessionID;
          if (sessionID) {
            upsertStatus(getDB(), sessionID, "error", 1);
          }
        }
      } catch {
        // Silently ignore DB errors -- don't break the OpenCode session.
      }
    },
  };
};

export { ClankSessionTracker };
export default ClankSessionTracker;
