package collectible

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

type chain struct {
	endpoint string
	address  string
	http     *http.Client
}

func newChain(endpoint, contract string) (*chain, error) {
	if endpoint == "" && contract == "" {
		return nil, nil
	}
	address, ok := wallet(contract)
	parsed, err := url.Parse(endpoint)
	if !ok || err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("valid WEB3_HTTP and WEB3_CONTRACT_ADDRESS are required together")
	}
	return &chain{endpoint: endpoint, address: address, http: &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func wallet(input string) (string, bool) {
	input = strings.TrimPrefix(strings.ToLower(input), "0x")
	if len(input) != 40 {
		return "", false
	}
	address, err := hex.DecodeString(input)
	return "0x" + input, err == nil && !allZero(address)
}

func (c *chain) signer(ctx context.Context, message, signature string) (string, error) {
	sig, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(signature), "0x"))
	if err != nil || len(sig) != 65 {
		return "", ErrSignature
	}
	v := sig[64]
	if v < 2 {
		v += 27
	}
	if v != 27 && v != 28 {
		return "", ErrSignature
	}
	hash := sha3.NewLegacyKeccak256()
	fmt.Fprintf(hash, "\x19Ethereum Signed Message:\n%d%s", len(message), message)
	var input [128]byte
	copy(input[:32], hash.Sum(nil))
	input[63] = v
	copy(input[64:], sig[:64])
	// EVM precompile 1 executes ecrecover over the exact EIP-191 digest and signature.
	result, err := c.call(ctx, "0x0000000000000000000000000000000000000001", hex.EncodeToString(input[:]), "latest")
	if err != nil {
		return "", err
	}
	if result == "0x" {
		return "", ErrSignature
	}
	return decodeAddress(result)
}

func (c *chain) owner(ctx context.Context, tokenID uint32) (string, error) {
	return c.ownerAt(ctx, tokenID, "latest")
}

func (c *chain) ownerAt(ctx context.Context, tokenID uint32, block string) (string, error) {
	result, err := c.call(ctx, c.address, fmt.Sprintf("6352211e%064x", tokenID), block)
	if err != nil {
		return "", err
	}
	return decodeAddress(result)
}

func (c *chain) finalizedBlock(ctx context.Context) (string, error) {
	var block struct {
		Number string `json:"number"`
	}
	if err := c.request(ctx, "eth_getBlockByNumber", []any{"finalized", false}, &block); err != nil {
		return "", err
	}
	if !strings.HasPrefix(block.Number, "0x") {
		return "", ErrUnavailable
	}
	if _, err := strconv.ParseUint(block.Number[2:], 16, 64); err != nil {
		return "", ErrUnavailable
	}
	return block.Number, nil
}

func (c *chain) inventory(ctx context.Context, owner, block string) (map[uint32]struct{}, error) {
	hash := sha3.NewLegacyKeccak256()
	hash.Write([]byte("tokensOfOwner(address)"))
	selector := hex.EncodeToString(hash.Sum(nil)[:4])
	result, err := c.call(ctx, c.address, selector+strings.Repeat("0", 24)+owner[2:], block)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(result, "0x") {
		return nil, ErrUnavailable
	}
	encoded, err := hex.DecodeString(result[2:])
	// ABI encoding: one offset word, one length word, then uint256 token IDs.
	if err != nil || len(encoded) < 64 || (len(encoded)-64)%32 != 0 || !allZero(encoded[:31]) ||
		encoded[31] != 32 || !allZero(encoded[32:56]) {
		return nil, ErrUnavailable
	}
	length := (len(encoded) - 64) / 32
	if binary.BigEndian.Uint64(encoded[56:64]) != uint64(length) {
		return nil, ErrUnavailable
	}
	tokens := make(map[uint32]struct{}, length)
	for index := 0; index < length; index++ {
		token := encoded[64+index*32 : 96+index*32]
		// The original API supports uint32 IDs. Higher token IDs have no metadata.
		if allZero(token[:28]) {
			tokens[binary.BigEndian.Uint32(token[28:])] = struct{}{}
		}
	}
	return tokens, nil
}

func decodeAddress(result string) (string, error) {
	if !strings.HasPrefix(result, "0x") {
		return "", ErrUnavailable
	}
	data, err := hex.DecodeString(result[2:])
	if err != nil || len(data) != 32 || !allZero(data[:12]) {
		return "", ErrUnavailable
	}
	address, ok := wallet(result[26:])
	if !ok {
		return "", ErrUnavailable
	}
	return address, nil
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func (c *chain) call(ctx context.Context, to, data, block string) (string, error) {
	var result string
	err := c.request(ctx, "eth_call", []any{map[string]string{"to": to, "data": "0x" + data}, block}, &result)
	if err == nil && result == "" {
		err = ErrUnavailable
	}
	return result, err
}

func (c *chain) request(ctx context.Context, method string, params []any, output any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	var reply struct {
		Version string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
	if err := decoder.Decode(&reply); err != nil || reply.Version != "2.0" || reply.ID != 1 || len(reply.Result) == 0 ||
		(len(reply.Error) != 0 && string(reply.Error) != "null") {
		return ErrUnavailable
	}
	if err := json.Unmarshal(reply.Result, output); err != nil {
		return ErrUnavailable
	}
	return nil
}
