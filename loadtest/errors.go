package loadtest

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
)

// ErrorClass categorizes a transport-level failure into a system-design
// failure mode. This makes it possible to distinguish, for example, backend
// saturation (timeouts) from a service being down (connection refused).
type ErrorClass string

const (
	ErrNone        ErrorClass = ""
	ErrTimeout     ErrorClass = "timeout"        // request exceeded its deadline
	ErrConnRefused ErrorClass = "conn_refused"   // nothing listening / backend down
	ErrConnReset   ErrorClass = "conn_reset"     // peer reset the connection
	ErrDNS         ErrorClass = "dns"            // name resolution failure
	ErrTLS         ErrorClass = "tls"            // TLS handshake / certificate error
	ErrCanceled    ErrorClass = "canceled"       // context canceled (test shutdown)
	ErrConnPool    ErrorClass = "conn_exhausted" // could not get a connection
	ErrOther       ErrorClass = "other"          // uncategorized transport error
)

// classifyError maps a transport error to an ErrorClass. The order of checks
// matters: more specific conditions are tested before generic ones.
func classifyError(err error) ErrorClass {
	if err == nil {
		return ErrNone
	}

	if errors.Is(err, context.Canceled) {
		return ErrCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}

	// net.Error timeout (covers client.Timeout and dial timeouts).
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrTimeout
	}

	// DNS resolution failures.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrDNS
	}

	// TLS handshake problems (certificate errors are matched by message
	// below; tls.CertificateVerificationError is only available in Go 1.20+).
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return ErrTLS
	}

	// Syscall-level connection errors.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return ErrConnRefused
	case strings.Contains(msg, "connection reset"):
		return ErrConnReset
	case strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate"):
		return ErrTLS
	case strings.Contains(msg, "no such host"):
		return ErrDNS
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return ErrTimeout
	case strings.Contains(msg, "cannot assign requested address") ||
		strings.Contains(msg, "too many open files") ||
		strings.Contains(msg, "connection pool"):
		return ErrConnPool
	}

	return ErrOther
}
