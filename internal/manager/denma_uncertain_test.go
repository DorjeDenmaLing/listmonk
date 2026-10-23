package manager

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/knadh/smtppool/v2"
)

// denmaFakeSMTP is a mail server that answers the message (after its final
// ".") with answer, or drops the connection if answer is "".
func denmaFakeSMTP(t *testing.T, answer string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				c.Write([]byte("220 fake\r\n"))
				data := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					switch {
					case data && line == ".":
						if answer == "" {
							return
						}
						c.Write([]byte(answer + "\r\n"))
						data = false
					case data:
					case strings.HasPrefix(line, "DATA"):
						data = true
						c.Write([]byte("354 go on\r\n"))
					case strings.HasPrefix(line, "QUIT"):
						c.Write([]byte("221 bye\r\n"))
						return
					default:
						c.Write([]byte("250 ok\r\n"))
					}
				}
			}(c)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// A send whose connection is lost after the message, before the server's
// answer, may have been delivered: it isn't tried again. A connect that
// fails, or the server's answer, is certain.
func TestDenmaUncertainSends(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	refused := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	for _, c := range []struct {
		name                 string
		port                 int
		uncertain, temporary bool
	}{
		{"dropped after the message", denmaFakeSMTP(t, ""), true, false},
		{"451 after the message", denmaFakeSMTP(t, "451 try later"), false, true},
		{"554 after the message", denmaFakeSMTP(t, "554 refused"), false, false},
		{"connection refused", refused, false, true},
	} {
		pool, err := smtppool.New(smtppool.Opt{Host: "127.0.0.1", Port: c.port, MaxConns: 1, MaxMessageRetries: 2,
			PoolWaitTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		err = pool.Send(smtppool.Email{From: "a@example.org", To: []string{"b@example.org"}, Subject: "x", Text: []byte("x")})
		pool.Close()
		if err == nil {
			t.Fatalf("%s: sent", c.name)
		}
		if u, tmp := DenmaUncertain(err), denmaTemporary(err); u != c.uncertain || tmp != c.temporary {
			t.Errorf("%s (%v): uncertain %v, temporary %v; want %v, %v", c.name, err, u, tmp, c.uncertain, c.temporary)
		}
	}
}
