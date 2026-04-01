package main

import (
	"fmt"
	"sync"
)

// queuedCall stores the original tool call parameters for auto-execution
// after an approval is granted.
type queuedCall struct {
	// ServerName is the downstream MCP server name.
	ServerName string
	// ToolName is the original (un-namespaced) tool name.
	ToolName string
	// QualifiedName is the namespaced tool name as seen by the agent.
	QualifiedName string
	// Arguments are the original tool call arguments.
	Arguments map[string]any
}

// callQueue stores tool calls that are pending approval for auto-execution.
// When a tool call is held for approval, the original request is queued by
// approval ID. When the agent later checks the approval status and it has
// been approved, the queued call is retrieved and executed automatically.
type callQueue struct {
	mu    sync.RWMutex
	calls map[string]queuedCall // approval_id → queued call
}

// newCallQueue creates an empty call queue.
func newCallQueue() *callQueue {
	return &callQueue{
		calls: make(map[string]queuedCall),
	}
}

// enqueue stores a tool call for later auto-execution, keyed by approval ID.
func (q *callQueue) enqueue(approvalID string, call queuedCall) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls[approvalID] = call
}

// dequeue retrieves and removes a queued tool call by approval ID.
// Returns the call and true if found, or zero value and false if not found.
func (q *callQueue) dequeue(approvalID string) (queuedCall, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	call, ok := q.calls[approvalID]
	if ok {
		delete(q.calls, approvalID)
	}
	return call, ok
}

// peek retrieves a queued tool call without removing it.
func (q *callQueue) peek(approvalID string) (queuedCall, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	call, ok := q.calls[approvalID]
	return call, ok
}

// len returns the number of queued calls.
func (q *callQueue) len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.calls)
}

// summary returns a human-readable description of a queued call.
func (qc queuedCall) summary() string {
	return fmt.Sprintf("%s (via %s)", qc.ToolName, qc.ServerName)
}
