package cache

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
)

type fakeClaudeReplayKVClient struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newFakeClaudeReplayKVClient() *fakeClaudeReplayKVClient {
	return &fakeClaudeReplayKVClient{values: make(map[string][]byte)}
}

func (c *fakeClaudeReplayKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, found := c.values[key]
	return append([]byte(nil), value...), found, nil
}

func (c *fakeClaudeReplayKVClient) KVSet(_ context.Context, key string, value []byte, _ homekv.KVSetOptions) (bool, error) {
	c.mu.Lock()
	c.values[key] = append([]byte(nil), value...)
	c.mu.Unlock()
	return true, nil
}

func (c *fakeClaudeReplayKVClient) KVDel(_ context.Context, keys ...string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var deleted int64
	for _, key := range keys {
		if _, found := c.values[key]; found {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *fakeClaudeReplayKVClient) KVCompareAndSwap(_ context.Context, key string, expected []byte, expectedExists bool, value []byte, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, found := c.values[key]
	if found != expectedExists || (found && !bytes.Equal(current, expected)) {
		return false, nil
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeClaudeReplayKVClient) KVExpire(_ context.Context, key string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, found := c.values[key]
	return found, nil
}

func useFakeClaudeThinkingReplayKVClient(t *testing.T, client *fakeClaudeReplayKVClient) {
	t.Helper()
	previous := currentClaudeThinkingReplayKVClient
	currentClaudeThinkingReplayKVClient = func() (claudeThinkingReplayKVClient, bool, error) {
		return client, true, nil
	}
	t.Cleanup(func() {
		currentClaudeThinkingReplayKVClient = previous
	})
}

func TestClaudeThinkingReplayAppendsAssistantTurns(t *testing.T) {
	client := newFakeClaudeReplayKVClient()
	useFakeClaudeThinkingReplayKVClient(t, client)

	const modelFamily = "claude:auth:model"
	const sessionKey = "execution:multi-turn"
	first := []byte(`[{"type":"thinking","thinking":"first","signature":"sig-1"},{"type":"tool_use","id":"toolu-1","name":"Read","input":{"path":"one"}}]`)
	second := []byte(`[{"type":"thinking","thinking":"second","signature":"sig-2"},{"type":"tool_use","id":"toolu-2","name":"Read","input":{"path":"two"}}]`)

	if !CacheClaudeThinkingReplayBestEffort(context.Background(), modelFamily, sessionKey, first) {
		t.Fatal("failed to seed first Claude replay turn")
	}
	_, snapshot, found, errGet := GetClaudeThinkingReplayWithSnapshotRequired(context.Background(), modelFamily, sessionKey)
	if errGet != nil || !found {
		t.Fatalf("initial Claude replay read = found %v, error %v", found, errGet)
	}
	replaced, errReplace := ReplaceClaudeThinkingReplayIfUnchanged(context.Background(), modelFamily, sessionKey, snapshot, second)
	if errReplace != nil || !replaced {
		t.Fatalf("append Claude replay turn = replaced %v, error %v", replaced, errReplace)
	}

	contents, found, errGet := GetClaudeThinkingReplayRequired(context.Background(), modelFamily, sessionKey)
	if errGet != nil || !found || len(contents) != 2 {
		t.Fatalf("Claude replay contents = %d, found %v, error %v; want two turns", len(contents), found, errGet)
	}
	if !bytes.Equal(contents[0], first) || !bytes.Equal(contents[1], second) {
		t.Fatalf("Claude replay contents lost ordering: got %s / %s", contents[0], contents[1])
	}
}
