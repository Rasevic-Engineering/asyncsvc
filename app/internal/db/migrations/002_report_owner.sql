-- Reports belong to the Firebase user who requested them. Rows created before
-- auth existed have no owner and are visible to admins only.
ALTER TABLE reports ADD COLUMN owner_uid text;
ALTER TABLE reports ADD COLUMN owner_email text;

CREATE INDEX reports_owner_created_idx ON reports (owner_uid, created_at DESC);
