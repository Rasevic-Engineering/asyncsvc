-- Download formats generated for each report. The CSV (file_key) is the
-- source; xlsx/pdf live next to it with the same name and another extension.
-- Reports created before this existed have CSV only.
ALTER TABLE reports ADD COLUMN formats text[] NOT NULL DEFAULT '{csv}';
