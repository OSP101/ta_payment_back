// Package antivirus scans uploaded files before they are stored.
//
// # WHY THIS IS NOT A THIRD-PARTY LIBRARY
//
// The obvious move is to import one of the Go clamd clients
// (baruwa-enterprise/clamd, lyimmi/go-clamd, …). They were considered and this
// is deliberately ~70 lines of protocol instead, for two reasons:
//
//  1. clamd's INSTREAM protocol is four lines of wire format with no parsing,
//     no crypto and no state machine. There is very little here to get wrong,
//     and correspondingly little value in outsourcing it.
//  2. This code sits directly in the trust path of a security control. A
//     dependency here is a supply-chain target with unusually high leverage: an
//     AV client that silently returns "clean" defeats the whole feature. The
//     most maintained option is also MPL-2.0, which is a licence question the
//     project should not inherit by accident.
//
// # WHAT THIS BUYS, AND WHAT IT DOES NOT
//
// ClamAV matches known signatures. It will catch commodity malware and the
// standard test files; it will NOT catch a novel exploit in a crafted PDF, and
// it is not a substitute for the PDF-only rule, the size cap, or the fact that
// nothing uploaded here is ever executed. Treat it as one more layer, not as
// "ความปลอดภัยสูงสุด" on its own.
//
// It also needs infrastructure, not just this file: a running clamd with a
// current signature database (see docker-compose.yml). A scanner with a stale
// database is a scanner that reports clean.
package antivirus

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ErrInfected is returned when the scanner names a signature. Signature is
// included for the audit trail — never shown to the uploader, who has no use
// for it and might be the attacker.
type ErrInfected struct {
	Signature string
}

func (e *ErrInfected) Error() string {
	return "file rejected by virus scan: " + e.Signature
}

// Scanner checks a file's bytes. Implementations must be safe for concurrent use.
type Scanner interface {
	// Scan reads r to completion. It returns nil when the file is clean,
	// *ErrInfected when a signature matched, and any other error when the scan
	// could not be completed — a distinction callers MUST preserve, because
	// "clean" and "could not tell" are not the same answer.
	Scan(ctx context.Context, r io.Reader) error
	// Enabled reports whether scanning is actually configured, so callers can
	// decide what to do about an unscanned upload rather than guessing.
	Enabled() bool
}

// Disabled is the no-op scanner used when no clamd address is configured. It
// reports Enabled() == false so the upload path can refuse or allow explicitly
// instead of silently treating "not configured" as "clean".
type Disabled struct{}

func (Disabled) Scan(context.Context, io.Reader) error { return nil }
func (Disabled) Enabled() bool                         { return false }

// ClamAV talks INSTREAM to a clamd daemon over TCP.
type ClamAV struct {
	addr    string
	timeout time.Duration
}

// New returns a ClamAV scanner, or Disabled when addr is empty.
func New(addr string, timeout time.Duration) Scanner {
	if strings.TrimSpace(addr) == "" {
		return Disabled{}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ClamAV{addr: addr, timeout: timeout}
}

func (c *ClamAV) Enabled() bool { return true }

// chunkSize stays well under clamd's default StreamMaxLength and its
// MaxScanSize accounting; 64 KiB is what the reference client uses.
const chunkSize = 64 * 1024

// Scan implements the INSTREAM command:
//
//	-> "zINSTREAM\0"
//	-> <uint32 big-endian length><bytes>   (repeated)
//	-> <uint32 zero>                       (end of stream)
//	<- "stream: OK\0" | "stream: <SIG> FOUND\0" | "... ERROR\0"
func (c *ClamAV) Scan(ctx context.Context, r io.Reader) error {
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("clamav: dial %s: %w", c.addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(c.timeout))
	}

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return fmt.Errorf("clamav: write command: %w", err)
	}

	buf := make([]byte, chunkSize)
	var sizeBuf [4]byte
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			binary.BigEndian.PutUint32(sizeBuf[:], uint32(n))
			if _, err := conn.Write(sizeBuf[:]); err != nil {
				return fmt.Errorf("clamav: write chunk header: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return fmt.Errorf("clamav: write chunk: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("clamav: read source: %w", readErr)
		}
	}
	// Zero-length chunk terminates the stream.
	binary.BigEndian.PutUint32(sizeBuf[:], 0)
	if _, err := conn.Write(sizeBuf[:]); err != nil {
		return fmt.Errorf("clamav: end stream: %w", err)
	}

	reply, err := bufio.NewReader(conn).ReadString('\x00')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("clamav: read reply: %w", err)
	}
	reply = strings.TrimRight(strings.TrimSpace(reply), "\x00")

	switch {
	case strings.HasSuffix(reply, "OK"):
		return nil
	case strings.HasSuffix(reply, "FOUND"):
		// "stream: Eicar-Signature FOUND" -> "Eicar-Signature"
		sig := strings.TrimSpace(strings.TrimSuffix(reply, "FOUND"))
		sig = strings.TrimSpace(strings.TrimPrefix(sig, "stream:"))
		if sig == "" {
			sig = "unknown"
		}
		return &ErrInfected{Signature: sig}
	case reply == "":
		return errors.New("clamav: empty reply")
	default:
		// Includes "... ERROR" and anything unrecognised. Never treated as
		// clean: an unparsed reply is a failed scan.
		return fmt.Errorf("clamav: unexpected reply: %q", reply)
	}
}
