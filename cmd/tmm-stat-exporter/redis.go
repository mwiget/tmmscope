// Minimal, dependency-free RESP2 client over mTLS — just the SCAN + GET the
// token collector needs to read iRule `table` counters out of DSSM/Redis.
// No third-party redis library (keeps the sidecar's dep surface at zero, like
// the tmstat reader). The connection is read-only and short-lived: one dial
// per scrape, closed when done.
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type redisClient struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

// dialRedis opens an mTLS RESP connection. certDir holds tls.crt/tls.key/ca.crt
// (the tmm DSSM client cert); serverName is the SNI/verify name (DSSM presents
// "dssm-svc"). Selects db if non-zero.
func dialRedis(addr, certDir, serverName string, db int, timeout time.Duration) (*redisClient, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "tls.crt"), filepath.Join(certDir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no CA certs in %s", certDir)
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c := &redisClient{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
	if db != 0 {
		if _, err := c.do("SELECT", strconv.Itoa(db)); err != nil {
			c.Close()
			return nil, fmt.Errorf("select %d: %w", db, err)
		}
	}
	return c, nil
}

func (c *redisClient) Close() { _ = c.conn.Close() }

// do writes a RESP array of bulk strings and reads one reply.
func (c *redisClient) do(args ...string) (interface{}, error) {
	fmt.Fprintf(c.w, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(c.w, "$%d\r\n%s\r\n", len(a), a)
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	return c.readReply()
}

// readReply parses one RESP2 value into string | int64 | []interface{} | nil.
func (c *redisClient) readReply() (interface{}, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, fmt.Errorf("short reply %q", line)
	}
	typ, body := line[0], line[1:len(line)-2] // strip \r\n
	switch typ {
	case '+':
		return body, nil
	case '-':
		return nil, fmt.Errorf("redis: %s", body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil // null bulk
		}
		buf := make([]byte, n+2)
		if _, err := readFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		arr := make([]interface{}, n)
		for i := 0; i < n; i++ {
			if arr[i], err = c.readReply(); err != nil {
				return nil, err
			}
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unknown RESP type %q", string(typ))
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// scanMatch returns every key matching pattern, walking the SCAN cursor.
func (c *redisClient) scanMatch(pattern string, count int) ([]string, error) {
	var keys []string
	cursor := "0"
	for {
		rep, err := c.do("SCAN", cursor, "MATCH", pattern, "COUNT", strconv.Itoa(count))
		if err != nil {
			return keys, err
		}
		arr, ok := rep.([]interface{})
		if !ok || len(arr) != 2 {
			return keys, fmt.Errorf("unexpected SCAN reply")
		}
		cursor, _ = arr[0].(string)
		if batch, ok := arr[1].([]interface{}); ok {
			for _, k := range batch {
				if s, ok := k.(string); ok {
					keys = append(keys, s)
				}
			}
		}
		if cursor == "0" {
			return keys, nil
		}
	}
}

// get returns the string value of key, or ("", false) if missing/wrong-type.
func (c *redisClient) get(key string) (string, bool) {
	rep, err := c.do("GET", key)
	if err != nil {
		return "", false // e.g. WRONGTYPE on the subtable index list
	}
	s, ok := rep.(string)
	return s, ok
}
