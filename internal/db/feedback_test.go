package db

import "testing"

func TestUpsertFeedbackRecordsIsAtomic(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "file:///tmp/upstream", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	records := []FeedbackRecord{
		{RunID: run.ID, ItemID: "first", ItemJSON: `{ "id": "first" }`, SourceHead: "head", Attempts: 1},
		{RunID: "missing-run", ItemID: "second", ItemJSON: `{ "id": "second" }`, SourceHead: "head", Attempts: 1},
	}
	if err := d.UpsertFeedbackRecords(records); err == nil {
		t.Fatal("expected foreign-key failure")
	}
	got, err := d.GetFeedbackRecords(run.ID)
	if err != nil {
		t.Fatalf("read feedback records: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("atomic batch left partial records: %#v", got)
	}
}
