// Package tlsfp shapes the outbound ClientHello. Go's crypto/tls has its own
// distinctive fingerprint; the legacy gateway used Python's OpenSSL stack, so a
// WAF can tell the two apart. This profile copies the ClientHello Python 3.13 /
// OpenSSL 3.5 sends (captured from the same host): cipher order, extension
// order, curves and point formats.
//
// The cipher list, extension order and point formats are byte-identical to the
// captured Python hello. The supported-groups list keeps x25519, P-256, P-384
// and P-521 but drops X25519MLKEM768, secp192r1 and the two FFDHE groups: uTLS
// cannot complete a handshake when they are advertised, and a hello that cannot
// connect is worse than one whose JA3 differs only in that field. Both hashes
// are asserted by the tests, and WB_TLS_PROFILE=go restores crypto/tls.
package tlsfp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// PythonJA3 is the fingerprint of ssl.create_default_context() on Python 3.13 /
// OpenSSL 3.5.9, as captured by a local ClientHello listener. ProfileJA3 is what
// this package sends: identical cipher/extension/point lists, but the curve
// list drops X25519MLKEM768 and the FFDHE/secp192 groups, which uTLS cannot
// complete a handshake with. See the package doc.
const (
	PythonJA3  = "e8df55bdfa49d878dc07891c00a025ce"
	ProfileJA3 = "4b70a84db63ecd1637e2ef292bbce5f0"
)

// Enabled reports whether the OpenSSL-shaped profile should be used.
// WB_TLS_PROFILE=go restores the standard library ClientHello.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_TLS_PROFILE"))) {
	case "go", "stdlib", "off", "0":
		return false
	}
	return true
}

// Spec is the ClientHello Python/OpenSSL sends.
func Spec() *utls.ClientHelloSpec {
	sigs := []utls.SignatureScheme{
		0x0403, // ecdsa_secp256r1_sha256
		0x0807, // ed25519
		0x0804, // rsa_pss_rsae_sha256
		0x0401, // rsa_pkcs1_sha256
		0x0503, // ecdsa_secp384r1_sha384
		0x0808, // ed448
		0x0805, // rsa_pss_rsae_sha384
		0x0501, // rsa_pkcs1_sha384
		0x0806, // rsa_pss_rsae_sha512
		0x0601, // rsa_pkcs1_sha512
		0x0809, // rsa_pss_pss_sha256
		0x080a, // rsa_pss_pss_sha384
		0x080b, // rsa_pss_pss_sha512
		0x0402, // dsa_sha256
	}
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, // TLS 1.3
			0xc02c, 0xc030, 0xc02b, 0xc02f, // ECDHE-ECDSA/RSA AES-GCM
			0xcca9, 0xcca8, // ECDHE CHACHA20
			0xc024, 0xc028, 0xc023, 0xc027, // static ECDH AES-GCM
			0x009f, 0x009e, 0x006b, 0x0067, // DHE
		},
		CompressionMethods: []uint8{0},
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
		Extensions: []utls.TLSExtension{
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SNIExtension{},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0, 1, 2}},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				29, // x25519
				23, // secp256r1
				30, // secp384r1
				24, // secp521r1
			}},
			&utls.SessionTicketExtension{},
			&utls.GenericExtension{Id: 22}, // encrypt-then-MAC, empty
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: sigs},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}}, // psk_dhe_ke
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519}, {Group: utls.CurveP256},
			}},
		},
	}
}

// Client wraps raw in a uTLS connection with the OpenSSL-shaped hello. It
// performs no handshake; the caller drives HandshakeContext with its timeout.
func Client(raw net.Conn, host string, roots *x509.CertPool) (*utls.UConn, error) {
	cfg := &utls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	u := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := u.ApplyPreset(Spec()); err != nil {
		return nil, err
	}
	return u, nil
}

// Handshake runs the client handshake with a context deadline supplied by the
// caller (via HandshakeContext).
func Handshake(ctx context.Context, raw net.Conn, host string, roots *x509.CertPool) (net.Conn, error) {
	cfg := &utls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	return HandshakeConfig(ctx, raw, cfg)
}

// HandshakeConfig is Handshake with an explicit uTLS config (tests).
func HandshakeConfig(ctx context.Context, raw net.Conn, cfg *utls.Config) (net.Conn, error) {
	u := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := u.ApplyPreset(Spec()); err != nil {
		return nil, err
	}
	if err := u.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return u, nil
}
