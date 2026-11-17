package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"time"
)

// DPoPKey is a P-256 private key in JWK form (RFC 7517). STS binds a
// refresh_token to the public key that first exchanged it, so the private key
// must travel with the account: a credential file without it can still chat
// until the STS token expires, and can never be refreshed again.
type DPoPKey map[string]string

type keyPair struct {
	private *ecdsa.PrivateKey
	public  map[string]string
}

func padded(b *big.Int, size int) []byte {
	out := make([]byte, size)
	raw := b.Bytes()
	if len(raw) > size {
		raw = raw[len(raw)-size:]
	}
	copy(out[size-len(raw):], raw)
	return out
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// NewDPoPKey generates a fresh P-256 key pair in JWK form.
func NewDPoPKey() (DPoPKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return DPoPKey{
		"kty": "EC",
		"crv": "P-256",
		"x":   b64(padded(key.PublicKey.X, 32)),
		"y":   b64(padded(key.PublicKey.Y, 32)),
		"d":   b64(padded(key.D, 32)),
	}, nil
}

func keyPairFrom(jwk DPoPKey) (*keyPair, error) {
	if jwk == nil || jwk["d"] == "" {
		return nil, fmt.Errorf("dpop key material missing")
	}
	if jwk["kty"] != "EC" || jwk["crv"] != "P-256" {
		return nil, fmt.Errorf("invalid DPoP JWK curve")
	}
	decode := func(field string) ([]byte, error) {
		raw, err := base64.RawURLEncoding.DecodeString(jwk[field])
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("invalid DPoP JWK %s", field)
		}
		return raw, nil
	}
	xRaw, err := decode("x")
	if err != nil {
		return nil, err
	}
	yRaw, err := decode("y")
	if err != nil {
		return nil, err
	}
	dRaw, err := decode("d")
	if err != nil {
		return nil, err
	}
	curve := elliptic.P256()
	x, y, d := new(big.Int).SetBytes(xRaw), new(big.Int).SetBytes(yRaw), new(big.Int).SetBytes(dRaw)
	if d.Sign() <= 0 || d.Cmp(curve.Params().N) >= 0 || !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("invalid DPoP JWK key material")
	}
	wantX, wantY := curve.ScalarBaseMult(dRaw)
	if wantX.Cmp(x) != 0 || wantY.Cmp(y) != 0 {
		return nil, fmt.Errorf("DPoP JWK public/private key mismatch")
	}
	return &keyPair{
		private: &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d},
		public:  map[string]string{"kty": "EC", "crv": "P-256", "x": jwk["x"], "y": jwk["y"]},
	}, nil
}

// SignProof builds the DPoP proof JWT for one token-endpoint POST. htu must be
// the exact target URL: STS rejects a proof whose htu differs.
func SignProof(jwk DPoPKey, htu string, now time.Time) (string, error) {
	kp, err := keyPairFrom(jwk)
	if err != nil {
		return "", err
	}
	header := map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": kp.public}
	headerJSON, _ := json.Marshal(header)
	payload := map[string]any{
		"htm": "POST",
		"htu": htu,
		"iat": now.Unix(),
		"jti": randomHex(16),
	}
	payloadJSON, _ := json.Marshal(payload)
	input := b64(headerJSON) + "." + b64(payloadJSON)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, kp.private, digest[:])
	if err != nil {
		return "", err
	}
	// Low-S normalisation: jose defaults to it and some verifiers insist.
	n := elliptic.P256().Params().N
	if half := new(big.Int).Rsh(n, 1); s.Cmp(half) > 0 {
		s.Sub(n, s)
	}
	return input + "." + b64(append(padded(r, 32), padded(s, 32)...)), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
