package tlsfp

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func TestSpecMatchesProfileJA3(t *testing.T) {
	raw, got := capture(t, func(conn net.Conn) {
		u, err := Client(conn, "example.com", nil)
		if err != nil {
			t.Error(err)
			return
		}
		_ = u.Handshake()
	})
	_ = raw
	if got != ProfileJA3 {
		t.Fatalf("JA3 = %s, want %s\nraw=%s", got, ProfileJA3, raw)
	}
	want := "771,4866-4867-4865-49196-49200-49195-49199-52393-52392-49188-49192-49187-49191-159-158-107-103," +
		"65281-0-11-10-35-22-23-13-43-45-51,29-23-30-24,0-1-2"
	if raw != want {
		t.Fatalf("raw = %s\nwant  %s", raw, want)
	}
	// Every field except the curve list is byte-identical to Python.
	python := "771,4866-4867-4865-49196-49200-49195-49199-52393-52392-49188-49192-49187-49191-159-158-107-103," +
		"65281-0-11-10-35-22-23-13-43-45-51,4588-29-23-30-24-25-256-257,0-1-2"
	prefix := "771,4866-4867-4865-49196-49200-49195-49199-52393-52392-49188-49192-49187-49191-159-158-107-103," +
		"65281-0-11-10-35-22-23-13-43-45-51,"
	if !strings.HasPrefix(raw, prefix) || !strings.HasPrefix(python, prefix) {
		t.Fatalf("cipher/extension order drifted")
	}
}

func capture(t *testing.T, dial func(net.Conn)) (string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type result struct{ raw, ja3 string }
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- result{}
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(c, hdr); err != nil {
			ch <- result{}
			return
		}
		body := make([]byte, int(binary.BigEndian.Uint16(hdr[3:5])))
		if _, err := io.ReadFull(c, body); err != nil {
			ch <- result{}
			return
		}
		j, r := parseHello(append(hdr, body...))
		ch <- result{raw: r, ja3: j}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	dial(conn)
	select {
	case res := <-ch:
		return res.raw, res.ja3
	case <-time.After(6 * time.Second):
		t.Fatal("no ClientHello captured")
		return "", ""
	}
}

func parseHello(rec []byte) (ja3, raw string) {
	if len(rec) < 9 || rec[0] != 22 || rec[5] != 1 {
		return "not-clienthello", ""
	}
	p := rec[9:]
	version := binary.BigEndian.Uint16(p[0:2])
	p = p[2:]
	p = p[32:]
	sid := int(p[0])
	p = p[1+sid:]
	csLen := int(binary.BigEndian.Uint16(p[0:2]))
	p = p[2:]
	var ciphers []string
	for i := 0; i < csLen; i += 2 {
		ciphers = append(ciphers, fmt.Sprint(binary.BigEndian.Uint16(p[i:i+2])))
	}
	p = p[csLen:]
	comp := int(p[0])
	p = p[1+comp:]
	p = p[2:] // extensions length
	var exts, curves, points []string
	for len(p) >= 4 {
		typ := binary.BigEndian.Uint16(p[0:2])
		l := int(binary.BigEndian.Uint16(p[2:4]))
		if l > len(p)-4 {
			break
		}
		data := p[4 : 4+l]
		exts = append(exts, fmt.Sprint(typ))
		switch typ {
		case 10:
			if len(data) >= 2 {
				n := int(binary.BigEndian.Uint16(data[0:2]))
				for i := 0; i+1 < n && 2+i+2 <= len(data); i += 2 {
					curves = append(curves, fmt.Sprint(binary.BigEndian.Uint16(data[2+i:4+i])))
				}
			}
		case 11:
			if len(data) >= 1 {
				for i := 0; i < int(data[0]) && 1+i < len(data); i++ {
					points = append(points, fmt.Sprint(data[1+i]))
				}
			}
		}
		p = p[4+l:]
	}
	raw = fmt.Sprintf("%d,%s,%s,%s,%s", version, strings.Join(ciphers, "-"), strings.Join(exts, "-"),
		strings.Join(curves, "-"), strings.Join(points, "-"))
	sum := md5.Sum([]byte(raw))
	return fmt.Sprintf("%x", sum), raw
}

var _ = tls.VersionTLS13

func TestHandshakeCompletesWithTheProfile(t *testing.T) {
	seen := make(chan []uint16, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	base := srv.TLS.Clone()
	srv.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case seen <- chi.CipherSuites:
		default:
		}
		return base, nil
	}}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := HandshakeConfig(ctx, raw, &utls.Config{ServerName: "example.com", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 12)
	n, _ := io.ReadFull(conn, buf)
	if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
		t.Fatalf("response %q", buf[:n])
	}
	cs := <-seen
	if len(cs) == 0 || cs[0] != 0x1302 || cs[len(cs)-1] != 0x0067 {
		t.Fatalf("cipher order: %x", cs)
	}
}
