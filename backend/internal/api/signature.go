package api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Challenge is the parsed form of the EIP-191 message the checkout page asks
// the user to sign. The server reconstructs the message from current state
// and matches field-by-field; any mismatch fails the request.
//
// See docs/checkout-sessions.md for the canonical format.
type Challenge struct {
	Prefix    string    // first line of the message; integrator-brandable
	SessionID string    // pins the signature to a specific session
	PlanID    string
	Wallet    string    // lowercase 0x…
	Contract  string    // lowercase 0x…
	ChainID   int64
	Issued    time.Time // RFC3339-UTC
}

// String renders the canonical message text. Used to verify a submitted
// signature: the server independently reconstructs the message and refuses
// the request if the submitted bytes don't match exactly.
func (c Challenge) String() string {
	return c.Prefix + "\n\n" +
		"Session: " + c.SessionID + "\n" +
		"Plan: " + c.PlanID + "\n" +
		"Wallet: " + strings.ToLower(c.Wallet) + "\n" +
		"Contract: " + strings.ToLower(c.Contract) + "\n" +
		"Chain: " + strconv.FormatInt(c.ChainID, 10) + "\n" +
		"Issued: " + c.Issued.UTC().Format("2006-01-02T15:04:05Z")
}

// ParseChallenge unmarshals a message into a Challenge. Returns an error only
// for structural problems (missing line, malformed integer, bad timestamp);
// field-level semantic checks (does the wallet match the recovered signer,
// is the contract the one we expect, etc.) are the caller's job.
func ParseChallenge(message string) (*Challenge, error) {
	lines := strings.Split(message, "\n")
	if len(lines) < 8 {
		return nil, errors.New("malformed: too few lines")
	}
	c := &Challenge{Prefix: lines[0]}
	if lines[1] != "" {
		return nil, errors.New("malformed: second line must be blank")
	}
	fields := map[string]*string{
		"Session":  &c.SessionID,
		"Plan":     &c.PlanID,
		"Wallet":   &c.Wallet,
		"Contract": &c.Contract,
	}
	var chainStr, issuedStr string
	others := map[string]*string{
		"Chain":  &chainStr,
		"Issued": &issuedStr,
	}
	for _, line := range lines[2:8] {
		colon := strings.Index(line, ": ")
		if colon < 0 {
			return nil, errors.New("malformed: line missing ': ' separator: " + line)
		}
		key, val := line[:colon], line[colon+2:]
		if dst, ok := fields[key]; ok {
			*dst = val
			continue
		}
		if dst, ok := others[key]; ok {
			*dst = val
			continue
		}
		return nil, errors.New("malformed: unknown field " + key)
	}
	if c.SessionID == "" || c.PlanID == "" || c.Wallet == "" || c.Contract == "" || chainStr == "" || issuedStr == "" {
		return nil, errors.New("malformed: missing one or more required fields")
	}
	cid, err := strconv.ParseInt(chainStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed: Chain: %w", err)
	}
	c.ChainID = cid
	t, err := time.Parse("2006-01-02T15:04:05Z", issuedStr)
	if err != nil {
		return nil, fmt.Errorf("malformed: Issued: %w", err)
	}
	c.Issued = t.UTC()
	c.Wallet = strings.ToLower(c.Wallet)
	c.Contract = strings.ToLower(c.Contract)
	return c, nil
}

// RecoverPersonalSigner returns the EVM address that produced `signatureHex`
// over `message` using the EIP-191 personal_sign scheme. Accepts both the v=27
// (yellow paper) and v=0 (raw secp256k1 recovery id) conventions.
func RecoverPersonalSigner(message, signatureHex string) (common.Address, error) {
	sig, err := hexutil.Decode(signatureHex)
	if err != nil {
		return common.Address{}, fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != 65 {
		return common.Address{}, fmt.Errorf("signature must be 65 bytes, got %d", len(sig))
	}
	// Wallets send v as 27/28 (yellow paper); SigToPub wants 0/1.
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	if sig[64] > 1 {
		return common.Address{}, fmt.Errorf("unexpected recovery id %d", sig[64])
	}
	prefix := []byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message)))
	hash := crypto.Keccak256(append(prefix, []byte(message)...))
	pub, err := crypto.SigToPub(hash, sig)
	if err != nil {
		return common.Address{}, fmt.Errorf("recover: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}
