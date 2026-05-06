package mcprouter

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"

	redis "github.com/redis/go-redis/v9"
)

type redisUserTokenCache struct {
	client *redis.Client
	gcm    cipher.AEAD
}

func (c *redisUserTokenCache) SetUserToken(ctx context.Context, sessionID, serverName, token string) error {
	enc, err := encrypt(c.gcm, []byte(token))
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}
	return c.client.HSet(ctx, userCredPrefix+sessionID, serverName, enc).Err()
}

func (c *redisUserTokenCache) GetUserToken(ctx context.Context, sessionID, serverName string) (string, bool, error) {
	enc, err := c.client.HGet(ctx, userCredPrefix+sessionID, serverName).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get token: %w", err)
	}
	plain, err := decrypt(c.gcm, enc)
	if err != nil {
		return "", false, fmt.Errorf("decrypt token: %w", err)
	}
	token := string(plain)
	if isExpiredJWT(token) {
		_ = c.DeleteUserToken(ctx, sessionID, serverName)
		return "", false, nil
	}
	return token, true, nil
}

func (c *redisUserTokenCache) DeleteUserToken(ctx context.Context, sessionID, serverName string) error {
	return c.client.HDel(ctx, userCredPrefix+sessionID, serverName).Err()
}
