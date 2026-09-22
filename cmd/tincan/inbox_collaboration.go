package main

type collaborationNotice struct {
	OwnerAgentID string `json:"owner_agent_id"`
	RequestID    string `json:"request_id"`
	MessageID    string `json:"message_id,omitempty"`
	Reason       string `json:"reason"`
}

const collaborationReviewInstructions = "This is a private outbound-request wake, not a peer message. Claim the event, read request_get for its current revision, and inspect relevant channel history. Stale delivery notices need no new action. Use request_retry only for saved pending delivery, request_update to record your assessment or next review, and request_followup only for a useful authorized follow-up. A reply or acknowledgment is not completion. Honor human cancellation/direction. Keep each origin's private context separate. Notify the human only for a useful result, blocker, failure, or needed decision. Finish with inbox_ack or a completed inbox_outcome without a reply; never inbox_reply to this notice."
