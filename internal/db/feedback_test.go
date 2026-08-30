package db

import (
	"path/filepath"
	"testing"
)

func TestReplaceFeedbackRecordsPersistsRestartSafeSnapshot(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "feedback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := insertFeedbackTestRun(database); err != nil {
		t.Fatal(err)
	}
	record := FeedbackRecord{RunID: "run", ItemID: "comment:1", ItemJSON: `{"ID":"comment:1"}`, SourceHead: "a", RepairedHead: "b", ValidatedHead: "b", Replied: true}
	if err := database.ReplaceFeedbackRecords("run", []FeedbackRecord{record}); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetFeedbackRecords("run")
	if err != nil || len(got) != 1 || got[0].ItemID != record.ItemID || !got[0].Replied || got[0].ValidatedHead != "b" {
		t.Fatalf("GetFeedbackRecords = %+v, %v", got, err)
	}
	if err := database.ReplaceFeedbackRecords("run", nil); err != nil {
		t.Fatal(err)
	}
	got, err = database.GetFeedbackRecords("run")
	if err != nil || len(got) != 0 {
		t.Fatalf("cleared records = %+v, %v", got, err)
	}
}

func insertFeedbackTestRun(database *DB) error {
	_, err := database.sql.Exec(`INSERT INTO repos(id,working_path,upstream_url,default_branch,created_at) VALUES('repo','/tmp/feedback-test','https://github.com/test/repo','main',1)`)
	if err != nil {
		return err
	}
	_, err = database.sql.Exec(`INSERT INTO runs(id,repo_id,branch,head_sha,base_sha,status,created_at,updated_at) VALUES('run','repo','feature','b','a','running',1,1)`)
	return err
}
