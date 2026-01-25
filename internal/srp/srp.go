// Package srp implements Apple's SRP-6a authentication protocol
package srp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"

	"golang.org/x/crypto/pbkdf2"
)

// RFC 5054 2048-bit group parameters
var (
	// N is the 2048-bit prime from RFC 5054
	N, _ = new(big.Int).SetString(
		"AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050"+
			"A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50"+
			"E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B8"+
			"55F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773B"+
			"CA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748"+
			"544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6"+
			"AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB6"+
			"94B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73",
		16)

	// g is the generator
	g = big.NewInt(2)

	// k = H(N || PAD(g))
	k *big.Int
)

func init() {
	// Calculate k = H(N || PAD(g))
	k = calculateK()
}

// Client represents an SRP client for Apple authentication
type Client struct {
	username string
	password *ApplePassword
	a        *big.Int // private ephemeral
	A        *big.Int // public ephemeral
}

// ApplePassword handles Apple's custom password encoding
type ApplePassword struct {
	password   string
	protocol   string
	salt       []byte
	iterations int
}

// NewApplePassword creates a new Apple password handler
func NewApplePassword(password string) *ApplePassword {
	return &ApplePassword{password: password}
}

// SetEncryptInfo sets the encryption parameters from server response
func (p *ApplePassword) SetEncryptInfo(protocol string, salt []byte, iterations int) {
	p.protocol = protocol
	p.salt = salt
	p.iterations = iterations
}

// Encode returns the PBKDF2-derived key as Apple expects it
func (p *ApplePassword) Encode() []byte {
	// First, SHA256 hash the password
	passwordHash := sha256.Sum256([]byte(p.password))

	var passwordDigest []byte
	if p.protocol == "s2k_fo" {
		// For s2k_fo, use hex-encoded hash
		passwordDigest = []byte(hex.EncodeToString(passwordHash[:]))
	} else {
		// For s2k, use raw bytes
		passwordDigest = passwordHash[:]
	}

	// Apply PBKDF2
	keyLength := 32
	return pbkdf2.Key(passwordDigest, p.salt, p.iterations, keyLength, sha256.New)
}

// NewClient creates a new SRP client
func NewClient(username string, password *ApplePassword) (*Client, error) {
	c := &Client{
		username: username,
		password: password,
	}

	// Generate random private ephemeral 'a' (256 bits)
	aBytes := make([]byte, 32)
	if _, err := rand.Read(aBytes); err != nil {
		return nil, fmt.Errorf("generate random: %w", err)
	}
	c.a = new(big.Int).SetBytes(aBytes)

	// Calculate A = g^a mod N
	c.A = new(big.Int).Exp(g, c.a, N)

	// Ensure A != 0 (mod N)
	if new(big.Int).Mod(c.A, N).Sign() == 0 {
		return nil, fmt.Errorf("invalid A value")
	}

	return c, nil
}

// GetPublicKey returns the base64-encoded public key A
func (c *Client) GetPublicKey() string {
	return base64.StdEncoding.EncodeToString(padToN(c.A))
}

// ProcessChallenge processes the server challenge and returns M1 and M2
func (c *Client) ProcessChallenge(saltB64, bB64 string) (m1B64, m2B64 string, err error) {
	// Decode server values
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return "", "", fmt.Errorf("decode salt: %w", err)
	}

	bBytes, err := base64.StdEncoding.DecodeString(bB64)
	if err != nil {
		return "", "", fmt.Errorf("decode B: %w", err)
	}
	B := new(big.Int).SetBytes(bBytes)

	// Verify B != 0 (mod N)
	if new(big.Int).Mod(B, N).Sign() == 0 {
		return "", "", fmt.Errorf("invalid B value")
	}

	// Calculate u = H(PAD(A) || PAD(B))
	u := calculateU(c.A, B)
	if u.Sign() == 0 {
		return "", "", fmt.Errorf("invalid u value")
	}

	// Get the encoded password (PBKDF2 derived)
	encodedPassword := c.password.Encode()

	// Calculate x using pysrp's formula with no_username_in_x:
	// x = H(salt || H(':' || password))
	// With no_username_in_x, username becomes empty but ':' remains
	innerHash := sha256.New()
	innerHash.Write([]byte(":"))
	innerHash.Write(encodedPassword)
	innerHashResult := innerHash.Sum(nil)

	outerHash := sha256.New()
	outerHash.Write(salt)
	outerHash.Write(innerHashResult)
	x := new(big.Int).SetBytes(outerHash.Sum(nil))

	// Calculate S = (B - k * g^x)^(a + u*x) mod N
	// First: g^x mod N
	gx := new(big.Int).Exp(g, x, N)

	// k * g^x mod N
	kgx := new(big.Int).Mul(k, gx)
	kgx.Mod(kgx, N)

	// B - k * g^x mod N
	base := new(big.Int).Sub(B, kgx)
	base.Mod(base, N)
	if base.Sign() < 0 {
		base.Add(base, N)
	}

	// a + u*x
	ux := new(big.Int).Mul(u, x)
	exp := new(big.Int).Add(c.a, ux)

	// S = base^exp mod N
	S := new(big.Int).Exp(base, exp, N)

	// Calculate session key K = H(S)
	K := sha256.Sum256(padToN(S))

	// Calculate M1 = H(H(N) xor H(g) || H(username) || salt || A || B || K)
	// Note: Apple's no_username_in_x means username is NOT used in x calculation
	// but it IS used in M1 calculation
	hN := sha256.Sum256(padToN(N))
	hg := sha256.Sum256(padToN(g))

	hNxorHg := make([]byte, 32)
	for i := 0; i < 32; i++ {
		hNxorHg[i] = hN[i] ^ hg[i]
	}

	hUsername := sha256.Sum256([]byte(c.username))

	m1Hash := sha256.New()
	m1Hash.Write(hNxorHg)
	m1Hash.Write(hUsername[:])
	m1Hash.Write(salt)
	m1Hash.Write(padToN(c.A))
	m1Hash.Write(padToN(B))
	m1Hash.Write(K[:])
	M1 := m1Hash.Sum(nil)

	// Calculate M2 = H(A || M1 || K)
	m2Hash := sha256.New()
	m2Hash.Write(padToN(c.A))
	m2Hash.Write(M1)
	m2Hash.Write(K[:])
	M2 := m2Hash.Sum(nil)

	return base64.StdEncoding.EncodeToString(M1), base64.StdEncoding.EncodeToString(M2), nil
}

// calculateK computes k = H(N || PAD(g))
func calculateK() *big.Int {
	h := sha256.New()
	h.Write(padToN(N))
	h.Write(padToN(g))
	return new(big.Int).SetBytes(h.Sum(nil))
}

// calculateU computes u = H(PAD(A) || PAD(B))
func calculateU(A, B *big.Int) *big.Int {
	h := sha256.New()
	h.Write(padToN(A))
	h.Write(padToN(B))
	return new(big.Int).SetBytes(h.Sum(nil))
}

// padToN pads a big.Int to the same byte length as N (256 bytes)
func padToN(num *big.Int) []byte {
	nLen := (N.BitLen() + 7) / 8 // 256 bytes for 2048-bit N
	numBytes := num.Bytes()
	if len(numBytes) >= nLen {
		return numBytes
	}
	padded := make([]byte, nLen)
	copy(padded[nLen-len(numBytes):], numBytes)
	return padded
}

// Verify M2 verifies the server's M2 response (optional, for extra security)
func VerifyM2(expectedM2B64, receivedM2B64 string) bool {
	expected, err1 := base64.StdEncoding.DecodeString(expectedM2B64)
	received, err2 := base64.StdEncoding.DecodeString(receivedM2B64)
	if err1 != nil || err2 != nil {
		return false
	}
	return hmac.Equal(expected, received)
}
