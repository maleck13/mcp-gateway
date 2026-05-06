package mcprouter

import (
	"context"
	"sync"
)

type memoryUserTokenCache struct {
	mu     sync.RWMutex
	tokens map[string]string // key: sessionID + ":" + serverName
}

func newMemoryUserTokenCache() *memoryUserTokenCache {
	return &memoryUserTokenCache{tokens: make(map[string]string)}
}

func memoryKey(sessionID, serverName string) string {
	return sessionID + ":" + serverName
}

func (c *memoryUserTokenCache) SetUserToken(_ context.Context, sessionID, serverName, token string) error {
	c.mu.Lock()
	c.tokens[memoryKey(sessionID, serverName)] = token
	c.mu.Unlock()
	return nil
}

func (c *memoryUserTokenCache) GetUserToken(_ context.Context, sessionID, serverName string) (string, bool, error) {
	c.mu.RLock()
	token, ok := c.tokens[memoryKey(sessionID, serverName)]
	c.mu.RUnlock()
	if !ok {
		return "", false, nil
	}
	if isExpiredJWT(token) {
		c.mu.Lock()
		delete(c.tokens, memoryKey(sessionID, serverName))
		c.mu.Unlock()
		return "", false, nil
	}
	return token, true, nil
}

func (c *memoryUserTokenCache) DeleteUserToken(_ context.Context, sessionID, serverName string) error {
	c.mu.Lock()
	delete(c.tokens, memoryKey(sessionID, serverName))
	c.mu.Unlock()
	return nil
}
