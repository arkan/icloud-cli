package srp

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
)

func TestCompareWithPython(t *testing.T) {
	// Same values as Python test
	username := "test@example.com"
	password := "testpassword"
	salt := []byte("testsalt12345678") // 16 bytes
	iterations := 20000
	protocol := "s2k_fo"

	fmt.Printf("Username: %s\n", username)
	fmt.Printf("Password: %s\n", password)
	fmt.Printf("Salt (hex): %s\n", hex.EncodeToString(salt))
	fmt.Printf("Salt (b64): %s\n", base64.StdEncoding.EncodeToString(salt))
	fmt.Printf("Iterations: %d\n", iterations)
	fmt.Printf("Protocol: %s\n", protocol)
	fmt.Println()

	// Create password handler
	srpPassword := NewApplePassword(password)
	srpPassword.SetEncryptInfo(protocol, salt, iterations)

	// Get PBKDF2 result
	pbkdf2Result := srpPassword.Encode()
	fmt.Printf("PBKDF2 result (hex): %s\n", hex.EncodeToString(pbkdf2Result))

	// Expected from Python
	expectedPBKDF2 := "b3d42c98ab65be74201c97990eaf0c1678a45a9f7ae5cbde81a12e4db42e8358"
	if hex.EncodeToString(pbkdf2Result) != expectedPBKDF2 {
		t.Errorf("PBKDF2 mismatch!\n  Go:     %s\n  Python: %s", hex.EncodeToString(pbkdf2Result), expectedPBKDF2)
	} else {
		fmt.Println("PBKDF2 matches Python!")
	}
	fmt.Println()

	// Create SRP client
	client, err := NewClient(username, srpPassword)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Get A
	APubKey := client.GetPublicKey()
	fmt.Printf("A (b64): %s\n", APubKey)

	// Use same fake B as Python
	fakeB := make([]byte, 256)
	for i := range fakeB {
		fakeB[i] = byte(i % 256)
	}
	fakeBB64 := base64.StdEncoding.EncodeToString(fakeB)
	saltB64 := base64.StdEncoding.EncodeToString(salt)

	// Process challenge
	m1B64, m2B64, err := client.ProcessChallenge(saltB64, fakeBB64)
	if err != nil {
		t.Fatalf("ProcessChallenge failed: %v", err)
	}

	m1, _ := base64.StdEncoding.DecodeString(m1B64)
	m2, _ := base64.StdEncoding.DecodeString(m2B64)

	fmt.Printf("M1 (hex): %s\n", hex.EncodeToString(m1))
	fmt.Printf("M1 (b64): %s\n", m1B64)
	fmt.Printf("M2 (hex): %s\n", hex.EncodeToString(m2))

	// Expected from Python
	expectedM1 := "81f897d401a5533a84c4e67d6a9b7849c0cd867aa79af0b79161c738f4ae00ae"
	expectedM2 := "c4d6e129d4cf1bfc889edc2fb8cd9c7530efc8fe3633be41fba8bcc49cfcff17"

	fmt.Println()
	if hex.EncodeToString(m1) == expectedM1 {
		fmt.Println("M1 matches Python!")
	} else {
		fmt.Printf("M1 mismatch!\n  Go:     %s\n  Python: %s\n", hex.EncodeToString(m1), expectedM1)
	}

	if hex.EncodeToString(m2) == expectedM2 {
		fmt.Println("M2 matches Python!")
	} else {
		fmt.Printf("M2 mismatch!\n  Go:     %s\n  Python: %s\n", hex.EncodeToString(m2), expectedM2)
	}
}
