package mail

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// serverCert issues a certificate for the given DNS name.
func (ca *testCA) serverCert(t *testing.T, dns string) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: dns},
		DNSNames:  []string{dns},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeSMTP is a tiny SMTP server for the sender tests. It records what it saw
// so tests can assert that nothing sensitive arrived before TLS.
type fakeSMTP struct {
	ln   net.Listener
	mode string // "starttls", "tls", "plain" (no STARTTLS offered), "silent"
	cert tls.Certificate
	// authMechs is advertised after EHLO, for example "PLAIN LOGIN".
	authMechs string

	mu       sync.Mutex
	events   []string // ordered, for example "EHLO", "STARTTLS", "AUTH:tls=true"
	messages []string // DATA payloads
	creds    []string // "user:pass"
	wg       sync.WaitGroup
}

func startFake(t *testing.T, mode string, cert tls.Certificate) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, mode: mode, cert: cert, authMechs: "PLAIN LOGIN"}
	f.wg.Add(1)
	go f.accept()
	t.Cleanup(func() { ln.Close(); f.wg.Wait() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) record(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeSMTP) snapshot() (events, messages, creds []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...), append([]string(nil), f.messages...), append([]string(nil), f.creds...)
}

func (f *fakeSMTP) has(prefix string) bool {
	ev, _, _ := f.snapshot()
	for _, e := range ev {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) accept() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer c.Close()
			f.serve(c)
		}()
	}
}

func (f *fakeSMTP) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if f.mode == "silent" {
		time.Sleep(10 * time.Second)
		return
	}
	isTLS := false
	if f.mode == "tls" {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			f.record("TLS-HANDSHAKE-FAILED")
			return
		}
		c, isTLS = tc, true
	}
	r := bufio.NewReader(c)
	w := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
	w("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			f.record("EHLO")
			w("250-fake")
			if f.mode == "starttls" && !isTLS {
				w("250-STARTTLS")
			}
			w("250-AUTH " + f.authMechs)
			w("250 8BITMIME")
		case cmd == "STARTTLS":
			f.record("STARTTLS")
			w("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
			if err := tc.Handshake(); err != nil {
				f.record("TLS-HANDSHAKE-FAILED")
				return
			}
			c, isTLS = tc, true
			r = bufio.NewReader(c)
			w = func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			f.record(fmt.Sprintf("AUTH:tls=%v", isTLS))
			parts := strings.Fields(line)
			if len(parts) == 3 {
				raw, _ := base64.StdEncoding.DecodeString(parts[2])
				f2 := strings.Split(string(raw), "\x00")
				if len(f2) == 3 {
					f.mu.Lock()
					f.creds = append(f.creds, f2[1]+":"+f2[2])
					f.mu.Unlock()
				}
			}
			w("235 ok")
		case cmd == "AUTH LOGIN":
			f.record(fmt.Sprintf("AUTH:tls=%v", isTLS))
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			u, _ := r.ReadString('\n')
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			p, _ := r.ReadString('\n')
			ub, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(u))
			pb, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(p))
			f.mu.Lock()
			f.creds = append(f.creds, string(ub)+":"+string(pb))
			f.mu.Unlock()
			w("235 ok")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			f.record(fmt.Sprintf("MAIL:tls=%v", isTLS))
			w("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			f.record("RCPT")
			w("250 ok")
		case cmd == "DATA":
			f.record(fmt.Sprintf("DATA:tls=%v", isTLS))
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".")) // undo dot-stuffing
			}
			f.mu.Lock()
			f.messages = append(f.messages, b.String())
			f.mu.Unlock()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		case cmd == "RSET", cmd == "NOOP":
			w("250 ok")
		default:
			w("502 unsupported")
		}
	}
}
