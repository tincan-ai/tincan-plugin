package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"slices"
	"strings"
)

// Requests are private attention/commitment records, not channel locks. Receipt
// is independent of completion; only explicit dependencies/resources serialize work.
type inboxRequest struct {
	AppliedUpdates map[string]bool  `json:"applied_updates,omitempty"`
	Event          inboxEvent       `json:"event"`
	Status         string           `json:"status"`
	Claim          *inboxClaim      `json:"claim,omitempty"`
	DeliveryError  string           `json:"delivery_error,omitempty"`
	Reply          *string          `json:"reply,omitempty"`
	Summary        string           `json:"summary,omitempty"`
	Context        string           `json:"context,omitempty"`
	Dependencies   []int64          `json:"dependencies,omitempty"`
	Resources      []string         `json:"resources,omitempty"`
	Approval       *requestApproval `json:"approval,omitempty"`
	Attempt        int              `json:"attempt"`
	LastClaim      string           `json:"last_claim,omitempty"`
	Decision       string           `json:"decision,omitempty"`
}
type requestApproval struct {
	ID         string `json:"id"`
	Question   string `json:"question"`
	Permission string `json:"permission"`
	Delivery   string `json:"delivery"`
	Source     string `json:"source,omitempty"`
}
type standingPolicy struct {
	Source     string   `json:"source"`
	Scope      string   `json:"scope"`
	Resources  []string `json:"resources,omitempty"`
	Boundaries []string `json:"boundaries,omitempty"`
	Revision   int      `json:"revision,omitempty"`
	MaxWorkers int      `json:"max_workers,omitempty"`
}

func (s *inboxState) migrate() error {
	if s.Version > 2 || s.After < 0 {
		return errors.New("unsupported or invalid inbox state")
	}
	if s.Version < 2 {
		if (s.Claim != nil || s.Reply != nil) && s.Pending == nil {
			return errors.New("invalid inbox cursor")
		}
		if s.Pending != nil {
			if s.Pending.Seq <= s.After {
				return errors.New("invalid inbox cursor")
			}
			status := "ready"
			if s.Claim != nil {
				status = "running"
			}
			if s.Reply != nil {
				status = "reply_pending"
			}
			s.Requests = append(s.Requests, inboxRequest{Event: *s.Pending, Status: status, Claim: s.Claim, Reply: s.Reply})
			s.After = s.Pending.Seq
		}
		s.Version = 2
	}
	seen := map[int64]bool{}
	for _, r := range s.Requests {
		if r.Event.Seq <= 0 || r.Event.Seq > s.After || seen[r.Event.Seq] {
			return errors.New("invalid request cursor")
		}
		seen[r.Event.Seq] = true
		switch r.Status {
		case "ready", "running", "awaiting_approval", "awaiting_information", "failed", "needs_recovery", "reply_pending", "completed", "declined", "cancelled":
		default:
			return errors.New("invalid request state")
		}
		if r.Status == "running" && r.Claim == nil {
			return errors.New("running request has no claim")
		}
		if (r.Status == "awaiting_approval" || r.Status == "awaiting_information") && (r.Approval == nil || r.Approval.ID == "" || r.Approval.Question == "" || r.Claim != nil) {
			return errors.New("invalid suspended request")
		}

		if r.Claim != nil && (r.Claim.WorkerID == "" || r.Claim.Token == "") {
			return errors.New("invalid inbox worker claim")
		}
	}
	// Put direct mentions first without disturbing chronological order within a class.
	slices.SortStableFunc(s.Requests, func(a, b inboxRequest) int {
		return requestPriority(b) - requestPriority(a)
	})
	return nil
}
func requestPriority(r inboxRequest) int {
	if r.Event.Kind == "join_requested" {
		return 2
	}
	if r.Event.Mentioned {
		return 1
	}
	return 0
}
func (s *inboxState) copy() inboxState {
	// Mutation is copy-on-write: a failed durable save must not change live state.
	b, _ := json.Marshal(s)
	var out inboxState
	_ = json.Unmarshal(b, &out)
	_ = out.migrate()
	return out
}
func (s *inboxState) request(seq int64) *inboxRequest {
	_ = s.migrate()
	for n := range s.Requests {
		if s.Requests[n].Event.Seq == seq {
			return &s.Requests[n]
		}
	}
	return nil
}
func terminal(status string) bool {
	return status == "completed" || status == "declined" || status == "cancelled"
}
func (s *inboxState) eligible(r *inboxRequest) bool {
	if r.Status != "ready" {
		return false
	}
	if lifecycleKind(r.Event) != "" {
		return true
	}
	limit := 4
	if s.Policy != nil && s.Policy.MaxWorkers > 0 {
		limit = s.Policy.MaxWorkers
	}
	running := 0
	for _, active := range s.Requests {
		if active.Status == "running" {
			running++
		}
	}
	if running >= limit {
		return false
	}

	for _, seq := range r.Dependencies {
		d := s.request(seq)
		if d == nil || d.Status != "completed" {
			return false
		}
	}
	for _, active := range s.Requests {
		if active.Claim != nil {
			for _, a := range active.Resources {
				for _, b := range r.Resources {
					if a == b {
						return false
					}
				}
			}
		}
	}
	return true
}
func (s *inboxState) runnable() *inboxRequest {
	_ = s.migrate()
	for n := range s.Requests {
		if s.eligible(&s.Requests[n]) {
			return &s.Requests[n]
		}
	}
	return nil
}

// Legacy fields remain an in-process projection for older embedding code. They
// are replaced by a legacy-reader rejection guard on disk; never use them to select an execution target.
func (s *inboxState) project() {
	r := s.runnable()
	if r == nil {
		for n := range s.Requests {
			if !terminal(s.Requests[n].Status) {
				r = &s.Requests[n]
				break
			}
		}
	}
	s.Pending, s.Claim, s.Reply = nil, nil, nil
	if r != nil {
		s.Pending, s.Claim, s.Reply = &r.Event, r.Claim, r.Reply
	}
}
func (i *inbox) ingest(e inboxEvent, valid bool) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	if e.Seq <= s.After {
		return false, nil
	}
	e.Mentioned = e.Kind == "message" && slices.Contains(e.Payload.Mentions, i.agent)
	accepted := valid && i.accepts(e)
	s.After = e.Seq
	if accepted {
		s.Requests = append(s.Requests, inboxRequest{Event: e, Status: "ready"})
	}
	return accepted, i.save(s)
}
func checkRequestClaim(r *inboxRequest, tokens []string) error {
	if r.Claim != nil && (len(tokens) != 1 || tokens[0] != r.Claim.Token) {
		return errors.New("mention belongs to a delegated worker; its claim is required")
	}
	if r.Claim == nil && len(tokens) > 0 && tokens[0] != "" {
		return errors.New("worker claim is no longer active")
	}
	if r.Status == "awaiting_approval" || r.Status == "awaiting_information" || r.Status == "needs_recovery" {
		return errors.New("request is suspended; use the controller decision/recovery path")
	}
	return nil
}

type commitmentUpdate struct {
	Seq     int64  `json:"seq"`
	Context string `json:"context"`
}
type commitmentReference struct {
	Seq       int64  `json:"seq"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	MessageID string `json:"message_id"`
}
type workerOutcome struct {
	Updates []commitmentUpdate `json:"updates,omitempty"`

	Status     string  `json:"status"`
	Reply      *string `json:"reply,omitempty"`
	Summary    string  `json:"summary,omitempty"`
	Context    string  `json:"context,omitempty"`
	Question   string  `json:"question,omitempty"`
	Permission string  `json:"permission,omitempty"`
}

func (i *inbox) outcome(seq int64, token string, o workerOutcome) (any, error) {
	if token == "" {
		return nil, errors.New("private worker claim required")
	}
	switch o.Status {
	case "completed", "awaiting_approval", "awaiting_information", "failed", "needs_recovery":
	default:
		return nil, errors.New("unsupported worker outcome")
	}
	if len(o.Context) > 65536 || len(o.Summary) > 4000 || len(o.Question) > 4000 || len(o.Permission) > 4000 {
		return nil, errors.New("worker outcome too large")
	}
	if (o.Status == "awaiting_approval" || o.Status == "awaiting_information") && strings.TrimSpace(o.Question) == "" {
		return nil, errors.New("waiting requires a concrete question")
	}
	if len(o.Updates) > 0 {
		if err := i.contextUpdates(seq, token, o.Updates); err != nil {
			return nil, err
		}
	}
	if o.Status == "completed" {
		if o.Reply != nil {
			return i.reply(seq, *o.Reply, token)
		}
		return map[string]any{"completed": seq}, i.ack(seq, token)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	r := s.request(seq)
	if r == nil {
		return nil, errors.New("unknown request")
	}
	// Lost response to a successful suspension is retry-safe, without taking a new claim.
	if r.LastClaim == token && token != "" && r.Status == o.Status {
		return map[string]any{"status": r.Status, "approval": r.Approval}, nil
	}
	if token == "" || r.Claim == nil || r.Claim.Token != token {
		return nil, errors.New("active claim required")
	}
	switch o.Status {
	case "awaiting_approval", "awaiting_information", "failed", "needs_recovery":
	default:
		return nil, errors.New("unsupported worker outcome")
	}
	if len(o.Context) > 65536 || len(o.Summary) > 4000 || len(o.Question) > 4000 || len(o.Permission) > 4000 {
		return nil, errors.New("worker outcome too large")
	}
	if (o.Status == "awaiting_approval" || o.Status == "awaiting_information") && strings.TrimSpace(o.Question) == "" {
		return nil, errors.New("waiting requires a concrete question")
	}
	if r.Reply != nil {
		return nil, errors.New("saved reply must be delivered, not replaced by another outcome")
	}
	r.Status, r.Summary = o.Status, o.Summary
	if o.Context != "" {
		r.Context += "\nWorker checkpoint: " + o.Context
	}
	if len(r.Context) > 65536 {
		return nil, errors.New("continuation context too large; retain a concise checkpoint")
	}
	if o.Status == "awaiting_approval" || o.Status == "awaiting_information" {
		r.Approval = &requestApproval{ID: core.ID("decision_"), Question: o.Question, Permission: o.Permission, Delivery: "pending"}
	}
	// Only structured safe suspension/failure releases ownership. Uncertain
	// execution retains its claim until a controller reconciles the worker.
	r.LastClaim = token
	if o.Status != "needs_recovery" {
		r.Claim = nil
	}
	s.Revision++
	if err := i.save(s); err != nil {
		return nil, err
	}
	return map[string]any{"status": r.Status, "approval": r.Approval}, nil
}
func (i *inbox) requests() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	for n := range s.Requests {
		s.Requests[n].LastClaim = ""
		if s.Requests[n].Claim != nil {
			s.Requests[n].Claim.Token = ""
		}
	}
	out, err := inboxWireObject(map[string]any{"requests": s.Requests, "policy": s.Policy, "controller_version": 2})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return out
}

// Controller calls only: decisions require an explicit user/operator source.
// Worker adapters expose neither this operation nor policy mutation to workers.
func (i *inbox) decision(seq int64, id, action, source, context string, stopped bool) error {
	if len(source) > 4096 || len(context) > 65536 {
		return errors.New("controller input too large")
	}
	if strings.TrimSpace(source) == "" {
		return errors.New("user/operator instruction source required; peer messages are not authorization")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	r := s.request(seq)
	if r == nil {
		return errors.New("unknown request")
	}
	if action == "presented" {
		if r.Approval == nil || r.Approval.ID != id {
			return errors.New("unknown decision")
		}
		if r.Approval.Delivery == "answered" {
			return nil
		}
		r.Approval.Delivery = "presented"
	} else if action == "approve" || action == "decline" || action == "answer" {
		if r.Approval == nil || r.Approval.ID != id {
			return errors.New("unknown decision")
		}
		if r.Approval.Source != "" {
			if r.Decision == action && r.Approval.Source == source {
				return nil
			}
			return errors.New("decision already resolved")
		}
		if r.Status != "awaiting_approval" && r.Status != "awaiting_information" {
			return errors.New("request is not awaiting a decision")
		}
		if action == "answer" && r.Status != "awaiting_information" {
			return errors.New("approval requires approve or decline")
		}
		r.Approval.Source, r.Approval.Delivery, r.Decision = source, "answered", action
		r.Status = "ready"
		if action == "decline" {
			reply := "I cannot proceed with this request."
			r.Reply = &reply
			r.Status = "reply_pending"
		}
		r.Context += "\nUser decision: " + context
	} else if action == "annotate" {
		// New information changes continuation context, never authorization/state.
		if terminal(r.Status) {
			return errors.New("request is resolved")
		}
		r.Context += "\nContext update (" + source + "): " + context
	} else if action == "resume" || action == "cancel" {
		if r.Reply != nil {
			return errors.New("saved reply requires delivery reconciliation")
		}
		if r.Claim != nil && !stopped {
			return errors.New("confirm prior execution stopped before recovery")
		}
		if terminal(r.Status) {
			return errors.New("request is already resolved")
		}
		if r.Status == "awaiting_approval" {
			return errors.New("use the matching approval decision")
		}
		r.Claim = nil
		r.Status = "ready"
		if action == "cancel" {
			r.Status = "cancelled"
		}
		r.Context += "\nController recovery: " + context
		r.Decision = source
	} else {
		return errors.New("unknown controller decision")
	}
	if len(r.Context) > 65536 {
		return errors.New("recovery context too large")
	}
	s.Revision++
	return i.save(s)
}
func (i *inbox) policy(p standingPolicy) error {
	if p.MaxWorkers < 0 || p.MaxWorkers > 32 {
		return errors.New("max_workers must be 1 to 32, or zero for default 4")
	}
	if len(p.Resources) > 100 || len(p.Boundaries) > 100 {
		return errors.New("policy too large")
	}
	for _, field := range append(append([]string{}, p.Resources...), p.Boundaries...) {
		if len(field) > 4096 {
			return errors.New("policy field too large")
		}
	}
	if strings.TrimSpace(p.Source) == "" || strings.TrimSpace(p.Scope) == "" || len(p.Source)+len(p.Scope) > 16384 {
		return errors.New("explicit user instruction source and bounded scope required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	p.Revision = 1
	if s.Policy != nil {
		p.Revision = s.Policy.Revision + 1
	}
	s.Policy = &p
	s.Revision++
	return i.save(s)
}

// Plan explicit dependencies/resources before starting work. Channels never
// implicitly serialize requests. Keys must be canonical host resource names.
func (i *inbox) plan(seq int64, dependencies []int64, resources []string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	r := s.request(seq)
	if r == nil || r.Status != "ready" {
		return errors.New("only ready messages may be planned")
	}
	if len(dependencies) > 100 || len(resources) > 100 {
		return errors.New("plan too large")
	}
	for _, key := range resources {
		if strings.TrimSpace(key) == "" || len(key) > 4096 {
			return errors.New("invalid resource key")
		}
	}
	r.Dependencies, r.Resources = dependencies, resources
	visiting := map[int64]bool{}
	visited := map[int64]bool{}
	var check func(int64) bool
	check = func(id int64) bool {
		if visiting[id] {
			return false
		}
		if visited[id] {
			return true
		}
		d := s.request(id)
		if d == nil {
			return false
		}
		visiting[id] = true
		for _, dep := range d.Dependencies {
			if !check(dep) {
				return false
			}
		}
		delete(visiting, id)
		visited[id] = true
		return true
	}
	if !check(seq) {
		return errors.New("dependencies must exist and be acyclic")
	}
	s.Revision++
	return i.save(s)
}

// A worker may attach peer clarifications to related commitments using its own
// claim. The source is derived from the saved event; updates never change scope,
// approvals, scheduling state or another request's execution ownership.
func (i *inbox) contextUpdates(seq int64, token string, updates []commitmentUpdate) error {
	if len(updates) > 20 {
		return errors.New("too many commitment updates")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.state.copy()
	source := s.request(seq)
	if source == nil {
		return errors.New("unknown source message")
	}
	if source.Claim == nil || source.Claim.Token != token {
		if source.LastClaim == token && token != "" {
			return nil
		}
		return errors.New("source worker claim required")
	}
	if source.AppliedUpdates == nil {
		source.AppliedUpdates = map[string]bool{}
	}
	for _, u := range updates {
		target := s.request(u.Seq)
		if target == nil || terminal(target.Status) || target.Event.ChannelID != source.Event.ChannelID {
			return errors.New("clarification target must be an unfinished commitment in the same channel")
		}
		if strings.TrimSpace(u.Context) == "" || len(u.Context) > 4000 {
			return errors.New("invalid clarification context")
		}
		key := fmt.Sprintf("%d:%d", source.Attempt, u.Seq)
		if source.AppliedUpdates[key] {
			continue
		}
		target.Context += "\nPeer clarification from " + source.Event.Payload.ID + ": " + u.Context
		if len(target.Context) > 65536 {
			return errors.New("clarification exceeds continuation limit")
		}
		source.AppliedUpdates[key] = true
	}
	return i.save(s)
}
