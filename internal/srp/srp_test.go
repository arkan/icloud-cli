package srp

import (
	"encoding/base64"
	"testing"
)

func TestApplePasswordEncode(t *testing.T) {
	// Test that PBKDF2 encoding works correctly
	password := NewApplePassword("testpassword")

	// Simulate server response
	salt := []byte("testsalt12345678") // 16 bytes
	password.SetEncryptInfo("s2k_fo", salt, 20000)

	encoded := password.Encode()

	// Should return 32 bytes
	if len(encoded) != 32 {
		t.Errorf("Expected 32 bytes, got %d", len(encoded))
	}

	// Test s2k protocol
	password2 := NewApplePassword("testpassword")
	password2.SetEncryptInfo("s2k", salt, 20000)
	encoded2 := password2.Encode()

	// s2k and s2k_fo should produce different results
	if string(encoded) == string(encoded2) {
		t.Error("s2k and s2k_fo should produce different results")
	}
}

func TestClientCreation(t *testing.T) {
	password := NewApplePassword("testpassword")
	client, err := NewClient("test@example.com", password)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Check that A is generated
	pubKey := client.GetPublicKey()
	if pubKey == "" {
		t.Error("Public key should not be empty")
	}

	// Verify it's valid base64
	_, err = base64.StdEncoding.DecodeString(pubKey)
	if err != nil {
		t.Errorf("Public key should be valid base64: %v", err)
	}
}

func TestProcessChallenge(t *testing.T) {
	password := NewApplePassword("testpassword")
	client, err := NewClient("test@example.com", password)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Simulate server response
	salt := []byte("testsalt12345678")
	password.SetEncryptInfo("s2k_fo", salt, 20000)

	// Create a fake server B (this won't be cryptographically valid,
	// but we're testing that the calculation runs without panic)
	saltB64 := base64.StdEncoding.EncodeToString(salt)

	// Generate a fake B value (256 bytes)
	fakeB := make([]byte, 256)
	for i := range fakeB {
		fakeB[i] = byte(i % 256)
	}
	bB64 := base64.StdEncoding.EncodeToString(fakeB)

	m1, m2, err := client.ProcessChallenge(saltB64, bB64)
	if err != nil {
		t.Fatalf("ProcessChallenge failed: %v", err)
	}

	// Check that M1 and M2 are generated
	if m1 == "" || m2 == "" {
		t.Error("M1 and M2 should not be empty")
	}

	// Verify they're valid base64
	m1Bytes, err := base64.StdEncoding.DecodeString(m1)
	if err != nil {
		t.Errorf("M1 should be valid base64: %v", err)
	}
	if len(m1Bytes) != 32 {
		t.Errorf("M1 should be 32 bytes (SHA256), got %d", len(m1Bytes))
	}

	m2Bytes, err := base64.StdEncoding.DecodeString(m2)
	if err != nil {
		t.Errorf("M2 should be valid base64: %v", err)
	}
	if len(m2Bytes) != 32 {
		t.Errorf("M2 should be 32 bytes (SHA256), got %d", len(m2Bytes))
	}
}

func TestPadToN(t *testing.T) {
	// N is 2048 bits = 256 bytes
	expectedLen := 256

	// Test padding of small number
	small := padToN(g)
	if len(small) != expectedLen {
		t.Errorf("Expected %d bytes, got %d", expectedLen, len(small))
	}

	// Test padding of N itself
	nPadded := padToN(N)
	if len(nPadded) != expectedLen {
		t.Errorf("Expected %d bytes, got %d", expectedLen, len(nPadded))
	}
}
