package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestChallengeRoundTrip(t *testing.T) {
	orig := Challenge{
		Prefix:    "erc20-subscription checkout",
		SessionID: "cs_abc123",
		PlanID:    "pro_monthly_usdc",
		Wallet:    "0xbeefbeefbeefbeefbeefbeefbeefbeefbeefbeef",
		Contract:  "0x89edce46cf42666e27a2fa099ee5df25b62736eb",
		ChainID:   84532,
		Issued:    time.Date(2026, 6, 3, 12, 1, 14, 0, time.UTC),
	}
	msg := orig.String()
	parsed, err := ParseChallenge(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Prefix != orig.Prefix ||
		parsed.SessionID != orig.SessionID ||
		parsed.PlanID != orig.PlanID ||
		parsed.Wallet != orig.Wallet ||
		parsed.Contract != orig.Contract ||
		parsed.ChainID != orig.ChainID ||
		!parsed.Issued.Equal(orig.Issued) {
		t.Fatalf("round-trip mismatch:\n  orig=%+v\n  got =%+v", orig, parsed)
	}
}

func TestParseChallengeRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"too short", "erc20\n\nSession: cs_x\nPlan: p\nWallet: 0x0\nContract: 0x0\n"},
		{"missing blank second line", "erc20\nSession: cs_x\nPlan: p\nWallet: 0x0\nContract: 0x0\nChain: 1\nIssued: 2026-01-01T00:00:00Z\n"},
		{"unknown field", "erc20\n\nSession: cs_x\nPlan: p\nWallet: 0x0\nContract: 0x0\nFoo: bar\nIssued: 2026-01-01T00:00:00Z\n"},
		{"bad chain int", "erc20\n\nSession: cs_x\nPlan: p\nWallet: 0x0\nContract: 0x0\nChain: notnum\nIssued: 2026-01-01T00:00:00Z\n"},
		{"bad issued time", "erc20\n\nSession: cs_x\nPlan: p\nWallet: 0x0\nContract: 0x0\nChain: 1\nIssued: nope\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseChallenge(tc.msg); err == nil {
				t.Fatalf("expected parse error for %q", tc.name)
			}
		})
	}
}

func TestRecoverPersonalSignerRoundTrip(t *testing.T) {
	// Deterministic key (NEVER use in production — it's just a test fixture).
	priv, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	if err != nil {
		t.Fatal(err)
	}
	expectedAddr := crypto.PubkeyToAddress(priv.PublicKey)

	message := "erc20-subscription checkout\n\nSession: cs_test\nPlan: pro\nWallet: " +
		strings.ToLower(expectedAddr.Hex()) +
		"\nContract: 0x0000000000000000000000000000000000000001\nChain: 84532\nIssued: 2026-06-03T12:01:14Z"

	// Sign per EIP-191 personal_sign.
	prefix := []byte("\x19Ethereum Signed Message:\n")
	digest := crypto.Keccak256(append(append(prefix, []byte(itoa(len(message)))...), []byte(message)...))
	sig, err := crypto.Sign(digest, priv)
	if err != nil {
		t.Fatal(err)
	}
	// crypto.Sign returns v as 0/1; wallets present 27/28. Test both forms.
	got, err := RecoverPersonalSigner(message, hexutil.Encode(sig))
	if err != nil {
		t.Fatalf("recover (v=0/1): %v", err)
	}
	if got != expectedAddr {
		t.Fatalf("recovered %s, want %s", got.Hex(), expectedAddr.Hex())
	}
	sig27 := make([]byte, len(sig))
	copy(sig27, sig)
	sig27[64] += 27
	got, err = RecoverPersonalSigner(message, hexutil.Encode(sig27))
	if err != nil {
		t.Fatalf("recover (v=27/28): %v", err)
	}
	if got != expectedAddr {
		t.Fatalf("recovered %s (v=27), want %s", got.Hex(), expectedAddr.Hex())
	}
}

func TestRecoverRejectsTamperedMessage(t *testing.T) {
	priv, _ := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	signerAddr := crypto.PubkeyToAddress(priv.PublicKey)
	message := "hello"
	prefix := []byte("\x19Ethereum Signed Message:\n")
	digest := crypto.Keccak256(append(append(prefix, []byte(itoa(len(message)))...), []byte(message)...))
	sig, _ := crypto.Sign(digest, priv)

	tampered, err := RecoverPersonalSigner("hellö", hexutil.Encode(sig))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	// Recovered key for a different message should NOT equal the signer.
	if tampered == signerAddr {
		t.Fatalf("tampered message must not recover to original signer")
	}
	_ = common.Address{} // keep import live
}

// itoa is a deliberate package-local helper so the test file doesn't drag in
// strconv just to format the EIP-191 message length.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
