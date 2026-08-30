// Package feedback owns the restart-safe ordering for PR feedback repairs.
package feedback

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type Classification string

const (
	Objective   Classification = "objective"
	Acknowledge Classification = "acknowledge"
	AskUser     Classification = "ask-user"
)

type Decision struct {
	ItemID         string
	Classification Classification
	ReplyBody      string
}

type Record struct {
	Item          scm.FeedbackItem
	SourceHead    string
	RepairedHead  string
	ValidatedHead string
	Replied       bool
}

type Reconciler struct {
	pending map[string]Record
}

func New() *Reconciler { return &Reconciler{pending: make(map[string]Record)} }

func MarshalItem(item scm.FeedbackItem) string {
	b, _ := json.Marshal(item)
	return string(b)
}

func UnmarshalItem(raw string) (scm.FeedbackItem, error) {
	var item scm.FeedbackItem
	err := json.Unmarshal([]byte(raw), &item)
	return item, err
}

func (r *Reconciler) Restore(records []Record) {
	for _, record := range records {
		if strings.TrimSpace(record.Item.ID) != "" {
			r.pending[record.Item.ID] = record
		}
	}
}

func (r *Reconciler) Records() []Record {
	ids := make([]string, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.pending[id])
	}
	return out
}

func (r *Reconciler) Pending(itemID string) (Record, bool) {
	record, ok := r.pending[itemID]
	return record, ok
}

func (r *Reconciler) Reserve(item scm.FeedbackItem, sourceHead string) bool {
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(sourceHead) == "" {
		return false
	}
	if _, exists := r.pending[item.ID]; exists {
		return false
	}
	r.pending[item.ID] = Record{Item: item, SourceHead: sourceHead}
	return true
}

func (r *Reconciler) MarkRepaired(itemID, head string) bool {
	record, ok := r.pending[itemID]
	if !ok || strings.TrimSpace(head) == "" {
		return false
	}
	record.RepairedHead = head
	record.ValidatedHead = ""
	r.pending[itemID] = record
	return true
}

func (r *Reconciler) Validate(head string) []Record {
	var ready []Record
	for id, record := range r.pending {
		if record.RepairedHead != head || record.Replied {
			continue
		}
		record.ValidatedHead = head
		r.pending[id] = record
		ready = append(ready, record)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Item.ID < ready[j].Item.ID })
	return ready
}

func (r *Reconciler) MarkReplied(itemID, head string) bool {
	record, ok := r.pending[itemID]
	if !ok || record.ValidatedHead != head {
		return false
	}
	record.Replied = true
	r.pending[itemID] = record
	return true
}

func (r *Reconciler) Retire(itemID string) { delete(r.pending, itemID) }
