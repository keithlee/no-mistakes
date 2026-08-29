// Package feedback contains the provider-neutral feedback reconciliation
// state machine. Provider adapters only read and write FeedbackItems; the
// policy here owns ordering, retry bounds, and the rule that no response is
// published before the validated head is known.
package feedback

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type Record struct {
	Item          scm.FeedbackItem
	SourceHead    string
	Attempts      int
	ValidatedHead string
	Replied       bool
	Repaired      bool
}

type Classification string

const (
	Objective Classification = "objective"
	AskUser   Classification = "ask-user"
)

type Action string

const (
	NoAction Action = "none"
	Restart  Action = "restart-review"
	Blocked  Action = "ask-user"
	Reply    Action = "reply"
	Resolve  Action = "resolve"
)

// Decision is supplied by the review/finding authority. Body is deliberately
// not interpreted by this package; callers must classify untrusted text using
// the normal review gate and may only return Objective when the requested
// change is unambiguous against intent and requirements.
type Decision struct {
	ItemID         string
	Classification Classification
	ReplyBody      string
}

type Pending struct {
	Item          scm.FeedbackItem
	SourceHead    string
	ReplyBody     string
	Attempts      int
	ValidatedHead string
	Replied       bool
	Repaired      bool
}

type Result struct {
	Action  Action
	Item    scm.FeedbackItem
	Reason  string
	Attempt int
}

type Reconciler struct {
	MaxRetries int
	pending    map[string]Pending
}

func New(maxRetries int) *Reconciler {
	if maxRetries < 1 {
		maxRetries = 1
	}
	return &Reconciler{MaxRetries: maxRetries, pending: make(map[string]Pending)}
}

func (r *Reconciler) Pending() map[string]Pending {
	out := make(map[string]Pending, len(r.pending))
	for id, p := range r.pending {
		out[id] = p
	}
	return out
}

func (r *Reconciler) Records() []Record {
	ids := make([]string, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		p := r.pending[id]
		out = append(out, Record{Item: p.Item, SourceHead: p.SourceHead, Attempts: p.Attempts, ValidatedHead: p.ValidatedHead, Replied: p.Replied, Repaired: p.Repaired})
	}
	return out
}

func (r *Reconciler) Restore(records []Record) {
	for _, record := range records {
		if strings.TrimSpace(record.Item.ID) == "" {
			continue
		}
		r.pending[record.Item.ID] = Pending{Item: record.Item, SourceHead: record.SourceHead, Attempts: record.Attempts, ValidatedHead: record.ValidatedHead, Replied: record.Replied, Repaired: record.Repaired}
	}
}

func MarshalItem(item scm.FeedbackItem) string { b, _ := json.Marshal(item); return string(b) }
func UnmarshalItem(raw string) (scm.FeedbackItem, error) {
	var item scm.FeedbackItem
	err := json.Unmarshal([]byte(raw), &item)
	return item, err
}

// Observe returns exactly one next action. New feedback revokes readiness;
// already pending feedback is not repeatedly sent to an agent while waiting
// for the restarted review/test/proof/proof-review gates.
func (r *Reconciler) Observe(snapshot scm.FeedbackSnapshot, policy scm.FeedbackPolicy, expectedHead string, decisions map[string]Decision) Result {
	if strings.TrimSpace(expectedHead) == "" || !strings.EqualFold(strings.TrimSpace(expectedHead), strings.TrimSpace(snapshot.HeadSHA)) {
		return Result{Action: Blocked, Reason: "feedback snapshot head is stale or unreadable"}
	}
	dispositioned := make(map[string]bool)
	for _, candidate := range snapshot.Items {
		if !strings.EqualFold(strings.TrimSpace(candidate.Author), strings.TrimSpace(policy.PRAuthor)) {
			continue
		}
		id, markerHead, disposition, ok := scm.ParseFeedbackDispositionMarker(candidate.Body)
		if ok && strings.EqualFold(markerHead, snapshot.HeadSHA) && strings.TrimSpace(disposition) != "" {
			dispositioned[id] = true
		}
	}
	for _, item := range snapshot.Items {
		if item.Resolved || !policy.InScope(item) {
			continue
		}
		if dispositioned[item.ID] {
			continue
		}
		pending, exists := r.pending[item.ID]
		if exists {
			if pending.SourceHead != snapshot.HeadSHA {
				return Result{Action: Blocked, Item: item, Reason: "feedback changed head while repair was pending"}
			}
			continue
		}
		decision, ok := decisions[item.ID]
		if !ok || decision.Classification == AskUser {
			return Result{Action: Blocked, Item: item, Reason: "feedback requires an intent/requirements decision"}
		}
		if decision.Classification != Objective {
			return Result{Action: Blocked, Item: item, Reason: "feedback classification is not objective"}
		}
		if r.MaxRetries < 1 {
			return Result{Action: Blocked, Item: item, Reason: "feedback repair retry budget is exhausted"}
		}
		r.pending[item.ID] = Pending{Item: item, SourceHead: snapshot.HeadSHA, ReplyBody: strings.TrimSpace(decision.ReplyBody), Attempts: 1}
		return Result{Action: Restart, Item: item, Attempt: 1, Reason: "objective feedback requires repair and full revalidation"}
	}
	return Result{Action: NoAction, Reason: "no new in-scope feedback"}
}

// Retry records a failed repair and requests another bounded attempt. It can
// never produce a reply or resolution action.
func (r *Reconciler) Retry(itemID string) Result {
	p, ok := r.pending[itemID]
	if !ok {
		return Result{Action: Blocked, Reason: "feedback repair is not registered"}
	}
	if p.Attempts >= r.MaxRetries {
		return Result{Action: Blocked, Item: p.Item, Attempt: p.Attempts, Reason: "feedback repair retry budget exhausted"}
	}
	p.Attempts++
	r.pending[itemID] = p
	return Result{Action: Restart, Item: p.Item, Attempt: p.Attempts, Reason: "retry objective feedback repair with full revalidation"}
}

// RepairedHead binds the pending item to the newly committed head. A repair
// never counts as validated by itself; it only changes the head that the later
// Test/Proof/ProofReview/CI gates must certify.
func (r *Reconciler) RepairedHead(itemID, head string) bool {
	p, ok := r.pending[itemID]
	if !ok || strings.TrimSpace(head) == "" {
		return false
	}
	p.SourceHead, p.ValidatedHead = head, ""
	p.Repaired = true
	r.pending[itemID] = p
	return true
}

// ValidationPassed is the only transition that can emit Reply. It requires
// every validated gate to pass on the same exact head; callers perform the
// actual provider writes in this returned order and call Disposition after a
// successful reply.
func (r *Reconciler) ValidationPassed(head string, ci, proofReview bool) []Result {
	if !ci || !proofReview {
		return nil
	}
	var out []Result
	ids := make([]string, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := r.pending[id]
		if p.SourceHead != head || !p.Repaired {
			continue
		}
		if p.Replied {
			if p.Item.Kind == scm.FeedbackInlineReview {
				out = append(out, Result{Action: Resolve, Item: p.Item, Reason: "reply already published; retry resolution"})
			}
			continue
		}
		if p.ValidatedHead != head {
			p.ValidatedHead = head
			r.pending[id] = p
		}
		out = append(out, Result{Action: Reply, Item: p.Item, Reason: fmt.Sprintf("validated head %s", head)})
	}
	return out
}

// Disposition marks the reply as complete. Inline resolution is a separate
// action and is returned only after the caller has confirmed the reply write.
func (r *Reconciler) Disposition(itemID, head string, replied bool) (Result, bool) {
	p, ok := r.pending[itemID]
	if !ok || !replied || p.ValidatedHead != head {
		return Result{Action: Blocked, Reason: "feedback reply is not bound to validated head"}, false
	}
	p.Replied = true
	r.pending[itemID] = p
	if p.Item.Kind == scm.FeedbackInlineReview {
		return Result{Action: Resolve, Item: p.Item, Reason: "reply published on validated head"}, true
	}
	delete(r.pending, itemID)
	return Result{Action: NoAction, Item: p.Item, Reason: "top-level feedback disposition marker published"}, true
}

// Resolved retires an inline item only after the provider confirms the thread
// resolution write. Keeping it pending across an API error makes recovery
// deterministic and prevents an unresolved thread from being forgotten.
func (r *Reconciler) Resolved(itemID string) { delete(r.pending, itemID) }
