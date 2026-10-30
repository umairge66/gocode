// Package session implements the gocode session "delivery" pipeline:
// the receive / hook / queue / drain lifecycle by which an inbound agent
// message is classified, run through hooks, and handed off to the session for
// enqueueing.
//
// CONCEPTUAL MODEL
//
//	A Delivery is an inbound agent message handed to session.receive. It
//	carries an Origin (the classified "peer kind" of the sender), the message
//	Content (plain text, or structured blocks), and optional Event / AgentID.
//
//	Receive runs the delivery through an ordered chain of Hooks. Each hook may
//	either PASS THE DELIVERY ON (invoking the next hook, ultimately the "core"
//	hook, which marks the delivery as ARRIVED) or ANSWER/CONSUME it, returning
//	a result WITHOUT passing it on.
//
//	Two invariants are enforced, each named by an error string:
//
//	  1. A hook must pass the delivery on. If the chain settles without the
//	     delivery ever arriving at the core, the delivery was consumed by a
//	     hook; the default reason is ErrHookDidNotPassOn:
//	       "a hook answered without passing the delivery on".
//
//	  2. Receive must queue the delivery. Once the delivery ARRIVES (hooks
//	     passed it on), the caller receives a Queueing handle and must call
//	     Queued() to signal the session actually drained/enqueued it. If the
//	     handle is instead disposed without Queued() (the drain refused or
//	     held the delivery), the pending hook completion fails with
//	     ErrNotQueued:
//	       "session.receive: the delivery was not queued (the drain refused or
//	        held it after the hooks passed it on)".
//
//	States of a received delivery: CONSUMED (a hook answered it), ARRIVED-then-
//	QUEUED (drained successfully), or ARRIVED-then-HELD/BLOCKED (disposed
//	without queueing -> ErrNotQueued).
package session
