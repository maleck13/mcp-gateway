package mcprouter

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	redis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/hkdf"
)

const (
	userCredPrefix = "usercred:"
	// hkdf info parameter — differentiates this derived key from other uses of the signing key
	hkdfInfo = "mcp-gateway-user-token-encryption"
)

// UserTokenCache stores per-user credentials scoped to a session and server.
type UserTokenCache interface {
	SetUserToken(ctx context.Context, sessionID, serverName, token string) error
	GetUserToken(ctx context.Context, sessionID, serverName string) (string, bool, error)
	DeleteUserToken(ctx context.Context, sessionID, serverName string) error
}

type userTokenCacheConfig struct {
	redisClient *redis.Client
	signingKey  string
}

// NewUserTokenCache returns an initialized UserTokenCache. Pass
// WithUserTokenRedisClient to use a Redis-backed store with AES-GCM
// encryption; otherwise an in-memory store is returned.
func NewUserTokenCache(opts ...func(*userTokenCacheConfig)) (UserTokenCache, error) {
	cfg := &userTokenCacheConfig{}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.redisClient != nil {
		key, err := deriveEncryptionKey(cfg.signingKey)
		if err != nil {
			return nil, fmt.Errorf("derive encryption key: %w", err)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create AES cipher: %w", err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create GCM: %w", err)
		}
		return &redisUserTokenCache{client: cfg.redisClient, gcm: gcm}, nil
	}
	return newMemoryUserTokenCache(), nil
}

// WithUserTokenRedisClient configures the cache to use Redis with encryption.
func WithUserTokenRedisClient(client *redis.Client) func(*userTokenCacheConfig) {
	return func(c *userTokenCacheConfig) {
		c.redisClient = client
	}
}

// WithUserTokenSigningKey sets the key used to derive AES encryption key via HKDF.
func WithUserTokenSigningKey(key string) func(*userTokenCacheConfig) {
	return func(c *userTokenCacheConfig) {
		c.signingKey = key
	}
}

// deriveEncryptionKey derives a 32-byte AES-256 key from the session signing
// key using HKDF (RFC 5869) with SHA-256.
func deriveEncryptionKey(signingKey string) ([]byte, error) {
	if signingKey == "" {
		return nil, fmt.Errorf("signing key is required for encryption")
	}
	hkdfReader := hkdf.New(sha256.New, []byte(signingKey), nil, []byte(hkdfInfo))
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, key); err != nil {
		return nil, err
	}
	return key, nil
}

// isExpiredJWT checks if a token looks like a JWT and whether it's expired.
// Returns true if the token is a JWT and has expired. Non-JWT tokens return false.
func isExpiredJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := &jwt.RegisteredClaims{}
	_, _, err := parser.ParseUnverified(token, claims)
	if err != nil {
		return false
	}
	if claims.ExpiresAt == nil {
		return false
	}
	return claims.ExpiresAt.Before(time.Now())
}

func encrypt(gcm cipher.AEAD, plaintext []byte) (string, error) {
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decrypt(gcm cipher.AEAD, encoded string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
