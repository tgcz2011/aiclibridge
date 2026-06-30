package adapter

import (
	"testing"
	"time"
)

func TestCodexWatchdogDebug(t *testing.T) {
	short := 50 * time.Millisecond
	c, fakeIO, msgCh, resCh := runFakeLifecycle(t, ExecOptions{}, short)

	go func() {
		for range msgCh {
		}
	}()

	stdin := c.stdin.(*fakeStdin)
	waitForRequests(t, stdin, 1)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	waitForRequests(t, stdin, 2)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"t-1"}}}`)
	waitForRequests(t, stdin, 3)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","id":3,"result":{}}`)
	fakeIO.writeStdout(t, `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"},"threadId":"t-1"}}`)

	// Sleep and then check the lifecycle's state.
	time.Sleep(200 * time.Millisecond)
	t.Logf("c.turnStarted=%v c.turnID=%q", c.turnStarted, c.turnID)
	t.Logf("c.notificationProtocol=%q", c.notificationProtocol)

	select {
	case res := <-resCh:
		t.Logf("got result: %+v", res)
	case <-time.After(500 * time.Millisecond):
		t.Logf("final timeout, no result")
	}
	fakeIO.closeStdout(t)
}
