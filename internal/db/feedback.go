package db

import "time"

type FeedbackRecord struct {
	RunID         string
	ItemID        string
	ItemJSON      string
	SourceHead    string
	Attempts      int
	ValidatedHead string
	Replied       bool
	Repaired      bool
	UpdatedAt     int64
}

func (d *DB) UpsertFeedbackRecord(r FeedbackRecord) error {
	if r.UpdatedAt == 0 {
		r.UpdatedAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(`INSERT INTO feedback_reconciliation(run_id,item_id,item_json,source_head,attempts,validated_head,replied,repaired,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,item_id) DO UPDATE SET item_json=excluded.item_json,source_head=excluded.source_head,attempts=excluded.attempts,validated_head=excluded.validated_head,replied=excluded.replied,repaired=excluded.repaired,updated_at=excluded.updated_at`, r.RunID, r.ItemID, r.ItemJSON, r.SourceHead, r.Attempts, r.ValidatedHead, r.Replied, r.Repaired, r.UpdatedAt)
	return err
}

func (d *DB) GetFeedbackRecords(runID string) ([]FeedbackRecord, error) {
	rows, err := d.sql.Query(`SELECT run_id,item_id,COALESCE(item_json,''),source_head,attempts,COALESCE(validated_head,''),replied,repaired,updated_at FROM feedback_reconciliation WHERE run_id=? ORDER BY item_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedbackRecord
	for rows.Next() {
		var r FeedbackRecord
		if err := rows.Scan(&r.RunID, &r.ItemID, &r.ItemJSON, &r.SourceHead, &r.Attempts, &r.ValidatedHead, &r.Replied, &r.Repaired, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) DeleteFeedbackRecord(runID, itemID string) error {
	_, err := d.sql.Exec(`DELETE FROM feedback_reconciliation WHERE run_id=? AND item_id=?`, runID, itemID)
	return err
}
