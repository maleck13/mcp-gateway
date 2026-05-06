package mcprouter

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func makeExpiredJWT(t *testing.T) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte("test-key"))
	require.NoError(t, err)
	return signed
}

func makeValidJWT(t *testing.T) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte("test-key"))
	require.NoError(t, err)
	return signed
}

func TestMemoryUserTokenCache_SetGetDelete(t *testing.T) {
	cache := newMemoryUserTokenCache()
	ctx := context.Background()

	_, ok, err := cache.GetUserToken(ctx, "sess1", "server1")
	require.NoError(t, err)
	require.False(t, ok, "missing key should return false")

	require.NoError(t, cache.SetUserToken(ctx, "sess1", "server1", "my-pat-token"))
	token, ok, err := cache.GetUserToken(ctx, "sess1", "server1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "my-pat-token", token)

	require.NoError(t, cache.DeleteUserToken(ctx, "sess1", "server1"))
	_, ok, err = cache.GetUserToken(ctx, "sess1", "server1")
	require.NoError(t, err)
	require.False(t, ok, "deleted key should return false")
}

func TestMemoryUserTokenCache_ScopedBySessionAndServer(t *testing.T) {
	cache := newMemoryUserTokenCache()
	ctx := context.Background()

	require.NoError(t, cache.SetUserToken(ctx, "sess1", "server1", "token-a"))
	require.NoError(t, cache.SetUserToken(ctx, "sess1", "server2", "token-b"))
	require.NoError(t, cache.SetUserToken(ctx, "sess2", "server1", "token-c"))

	tok, ok, _ := cache.GetUserToken(ctx, "sess1", "server1")
	require.True(t, ok)
	require.Equal(t, "token-a", tok)

	tok, ok, _ = cache.GetUserToken(ctx, "sess1", "server2")
	require.True(t, ok)
	require.Equal(t, "token-b", tok)

	tok, ok, _ = cache.GetUserToken(ctx, "sess2", "server1")
	require.True(t, ok)
	require.Equal(t, "token-c", tok)
}

func TestMemoryUserTokenCache_ExpiredJWT(t *testing.T) {
	cache := newMemoryUserTokenCache()
	ctx := context.Background()

	expired := makeExpiredJWT(t)
	require.NoError(t, cache.SetUserToken(ctx, "sess1", "server1", expired))

	_, ok, err := cache.GetUserToken(ctx, "sess1", "server1")
	require.NoError(t, err)
	require.False(t, ok, "expired JWT should be treated as cache miss")

	// verify the token was deleted
	cache.mu.RLock()
	_, exists := cache.tokens[memoryKey("sess1", "server1")]
	cache.mu.RUnlock()
	require.False(t, exists, "expired JWT should be removed from store")
}

func TestMemoryUserTokenCache_ValidJWT(t *testing.T) {
	cache := newMemoryUserTokenCache()
	ctx := context.Background()

	valid := makeValidJWT(t)
	require.NoError(t, cache.SetUserToken(ctx, "sess1", "server1", valid))

	token, ok, err := cache.GetUserToken(ctx, "sess1", "server1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, valid, token)
}

func TestMemoryUserTokenCache_OpaqueToken(t *testing.T) {
	cache := newMemoryUserTokenCache()
	ctx := context.Background()

	require.NoError(t, cache.SetUserToken(ctx, "sess1", "server1", "ghp_abc123def456"))

	token, ok, err := cache.GetUserToken(ctx, "sess1", "server1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ghp_abc123def456", token)
}

func TestIsExpiredJWT(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		expired bool
	}{
		{"opaque PAT", "ghp_abc123", false},
		{"random string", "not-a-jwt", false},
		{"empty string", "", false},
		{"expired JWT", makeExpiredJWT(t), true},
		{"valid JWT", makeValidJWT(t), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expired, isExpiredJWT(tt.token))
		})
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key, err := deriveEncryptionKey("test-signing-key")
	require.NoError(t, err)
	require.Len(t, key, 32)

	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	plaintext := "ghp_supersecrettoken123"
	encrypted, err := encrypt(gcm, []byte(plaintext))
	require.NoError(t, err)
	require.NotEqual(t, plaintext, encrypted, "encrypted should differ from plaintext")

	decrypted, err := decrypt(gcm, encrypted)
	require.NoError(t, err)
	require.Equal(t, plaintext, string(decrypted))
}

func TestEncryptProducesDifferentCiphertext(t *testing.T) {
	key, _ := deriveEncryptionKey("test-key")
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)

	enc1, err := encrypt(gcm, []byte("same-token"))
	require.NoError(t, err)
	enc2, err := encrypt(gcm, []byte("same-token"))
	require.NoError(t, err)
	require.NotEqual(t, enc1, enc2, "same plaintext should produce different ciphertext due to random nonce")
}

func TestDecryptWithWrongKey(t *testing.T) {
	key1, _ := deriveEncryptionKey("key-one")
	block1, _ := aes.NewCipher(key1)
	gcm1, _ := cipher.NewGCM(block1)

	key2, _ := deriveEncryptionKey("key-two")
	block2, _ := aes.NewCipher(key2)
	gcm2, _ := cipher.NewGCM(block2)

	encrypted, err := encrypt(gcm1, []byte("secret"))
	require.NoError(t, err)

	_, err = decrypt(gcm2, encrypted)
	require.Error(t, err, "decrypting with wrong key should fail")
}

func TestDeriveEncryptionKey_EmptyKey(t *testing.T) {
	_, err := deriveEncryptionKey("")
	require.Error(t, err)
}

func TestNewUserTokenCache_InMemory(t *testing.T) {
	cache, err := NewUserTokenCache()
	require.NoError(t, err)
	require.IsType(t, &memoryUserTokenCache{}, cache)
}
