package corepipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"piumy-gateway/internal/gateway"
)

// fakeGateway implements gateway.Gateway for tests — with it, the whole
// pipeline is testable with no open-wa involved.
type fakeGateway struct {
	mu        sync.Mutex
	inbound   chan gateway.Inbound
	connected bool
	sent      []sentCall
	sendErr   error
	stopped   bool
	// sendDelay makes Send block before returning — used to deterministically
	// widen the window where processOutbox is "in flight" mid-Send, for
	// tests that need to provoke a shutdown race on purpose (see
	// TestControllerStopWaitsForInFlightOutboxSend).
	sendDelay time.Duration
	// sendStarted, if non-nil, gets a non-blocking signal the INSTANT Send
	// is entered — before sendDelay's sleep. A test that needs to know
	// "Send is now in flight" waits on this instead of a fixed sleep
	// (ct-2026-07-18-1507: the fixed-sleep version was the actual source
	// of TestControllerStopWaitsForInFlightOutboxSend's flakiness — under
	// scheduler load the ticker+dispatch+composing delay chain could still
	// be short of reaching Send by the time the sleep elapsed, so Stop()
	// legitimately had nothing to wait for and returned "too fast").
	sendStarted chan struct{}

	markRead []markReadCall
}

type sentCall struct {
	toJID, text string
}

type markReadCall struct {
	chatJID string
	msgIDs  []string
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{inbound: make(chan gateway.Inbound, 10), connected: true}
}

func (f *fakeGateway) Start(ctx context.Context) error { return nil }

func (f *fakeGateway) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.stopped = true
		close(f.inbound)
	}
}

func (f *fakeGateway) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeGateway) setConnected(c bool) {
	f.mu.Lock()
	f.connected = c
	f.mu.Unlock()
}

func (f *fakeGateway) setSendErr(err error) {
	f.mu.Lock()
	f.sendErr = err
	f.mu.Unlock()
}

func (f *fakeGateway) setSendDelay(d time.Duration) {
	f.mu.Lock()
	f.sendDelay = d
	f.mu.Unlock()
}

func (f *fakeGateway) setSendStarted(ch chan struct{}) {
	f.mu.Lock()
	f.sendStarted = ch
	f.mu.Unlock()
}

func (f *fakeGateway) Inbound() <-chan gateway.Inbound { return f.inbound }

func (f *fakeGateway) Send(ctx context.Context, toJID, text string) (gateway.SendResult, error) {
	f.mu.Lock()
	delay := f.sendDelay
	started := f.sendStarted
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if delay > 0 {
		time.Sleep(delay) // outside the lock: Connected()/setConnected must not block on this
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return gateway.SendResult{}, f.sendErr
	}
	f.sent = append(f.sent, sentCall{toJID, text})
	return gateway.SendResult{MsgID: fmt.Sprintf("fake-%d", len(f.sent)), TS: time.Now().Unix()}, nil
}

func (f *fakeGateway) sentCalls() []sentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentCall, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeGateway) SetTyping(ctx context.Context, toJID string, on bool) error { return nil }

func (f *fakeGateway) MarkRead(ctx context.Context, chatJID string, msgIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markRead = append(f.markRead, markReadCall{chatJID, msgIDs})
	return nil
}

func (f *fakeGateway) markReadCalls() []markReadCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]markReadCall, len(f.markRead))
	copy(out, f.markRead)
	return out
}

func (f *fakeGateway) MarkDelivered(ctx context.Context, chatJID string, msgIDs []string) error {
	return nil
}

func (f *fakeGateway) QRChannel(ctx context.Context) (<-chan string, error) {
	return nil, nil
}

var errFakeSend = errors.New("fake send error")
