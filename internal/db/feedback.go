package db

import "time"

type FeedbackRecord struct {
	RunID         string
	ItemID        string
	ItemJSON      string
	SourceHead    string
	RepairedHead  string
	ValidatedHead string
	Replied       bool
	UpdatedAt     int64
}

func (d *DB) ReplaceFeedbackRecords(runID string, records []FeedbackRecord) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM feedback_reconciliation WHERE run_id=?`, runID); err != nil {
		return err
	}
	const query = `INSERT INTO feedback_reconciliation(run_id,item_id,item_json,source_head,repaired_head,validated_head,replied,updated_at) VALUES(?,?,?,?,?,?,?,?)`
	for _, record := range records {
		if record.UpdatedAt == 0 {
			record.UpdatedAt = time.Now().Unix()
		}
		if _, err := tx.Exec(query, runID, record.ItemID, record.ItemJSON, record.SourceHead, record.RepairedHead, record.ValidatedHead, record.Replied, record.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetFeedbackRecords(runID string) ([]FeedbackRecord, error) {
	rows, err := d.sql.Query(`SELECT run_id,item_id,item_json,source_head,COALESCE(repaired_head,''),COALESCE(validated_head,''),replied,updated_at FROM feedback_reconciliation WHERE run_id=? ORDER BY item_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []FeedbackRecord
	for rows.Next() {
		var record FeedbackRecord
		if err := rows.Scan(&record.RunID, &record.ItemID, &record.ItemJSON, &record.SourceHead, &record.RepairedHead, &record.ValidatedHead, &record.Replied, &record.UpdatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
