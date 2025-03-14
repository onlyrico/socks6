package socks6

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/pion/dtls/v2"
	"github.com/studentmain/socks6/common"
	"github.com/studentmain/socks6/common/lg"
	"github.com/studentmain/socks6/internal"
	"github.com/studentmain/socks6/message"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func relayConnTuple(c, r net.Conn) string {
	return fmt.Sprintf("%s <==>> %s", conn5TupleIn(c), conn5TupleOut(r))
}

func conn3Tuple(c net.Conn) string {
	return fmt.Sprintf("%s(%s)", c.RemoteAddr().String(), connNet(c))
}

func conn5TupleIn(c net.Conn) string {
	return fmt.Sprintf("%s -(%s)-> %s", c.RemoteAddr().String(), connNet(c), c.LocalAddr().String())
}

func conn5TupleOut(c net.Conn) string {
	return fmt.Sprintf("%s -(%s)-> %s", c.LocalAddr().String(), connNet(c), c.RemoteAddr().String())
}

func connNet(c net.Conn) string {
	n := "?"
	switch c.(type) {
	case *net.TCPConn:
		n = "tcp"
	case *net.UDPConn:
		n = "udp"
	case *net.UnixConn:
		n = "unix"
	case *tls.Conn:
		n = "tls"
	case *dtls.Conn:
		n = "dtls"
	}
	return n
}

func relay(ctx context.Context, c, r net.Conn, timeout time.Duration) error {
	var wg sync.WaitGroup
	wg.Add(3)
	var err error = nil
	errCh := make(chan error, 4)
	ctx2, cancel := context.WithCancel(ctx)
	defer c.Close()
	defer r.Close()
	defer cancel()

	go func() {
		defer wg.Done()
		select {
		case <-ctx2.Done():
			err = ctx2.Err()
		case err = <-errCh:
			cancel()
		}
	}()

	go func() {
		defer wg.Done()
		e := relayOneDirection(c, r, timeout)
		// if already recorded an err, then another direction is already closed
		if e != nil {
			errCh <- e
		}
	}()
	go func() {
		defer wg.Done()
		e := relayOneDirection(r, c, timeout)
		if e != nil {
			errCh <- e
		}
	}()
	wg.Wait()

	if err == io.EOF {
		return nil
	}
	return err
}

func relayOneDirection(c1, c2 net.Conn, timeout time.Duration) error {
	var done error = nil
	buf := internal.BytesPool4k.Rent()
	defer internal.BytesPool4k.Return(buf)

	// copy pasted from io.Copy with some modify
	for {
		c1.SetReadDeadline(time.Now().Add(timeout))
		nRead, eRead := c1.Read(buf)
		if done != nil {
			return done
		}

		if nRead > 0 {
			c2.SetWriteDeadline(time.Now().Add(timeout))
			nWrite, eWrite := c2.Write(buf[:nRead])
			if done != nil {
				return done
			}

			if eWrite != nil {
				return eWrite
			}
			if nRead != nWrite {
				return io.ErrShortWrite
			}
		}
		if eRead != nil {
			return eRead
		}
	}
}

// getReplyCode convert dial error to socks6 error code
func getReplyCode(err error) message.ReplyCode {
	if err == nil {
		return message.OperationReplySuccess
	}
	netErr, ok := err.(net.Error)
	if !ok {
		lg.Warning(err)
		return message.OperationReplyServerFailure
	}
	if netErr.Timeout() {
		return message.OperationReplyTimeout
	}
	opErr, ok := netErr.(*net.OpError)
	if !ok {
		return message.OperationReplyServerFailure
	}

	switch t := opErr.Err.(type) {
	case *os.SyscallError:
		errno, ok := t.Err.(syscall.Errno)
		if !ok {
			return message.OperationReplyServerFailure
		}
		// windows use windows.WSAExxxx error code, so this is necessary
		switch common.ConvertSocketErrno(errno) {
		case syscall.ENETUNREACH:
			return message.OperationReplyNetworkUnreachable
		case syscall.EHOSTUNREACH:
			return message.OperationReplyHostUnreachable
		case syscall.ECONNREFUSED:
			return message.OperationReplyConnectionRefused
		case syscall.ETIMEDOUT:
			return message.OperationReplyTimeout
		default:
			return message.OperationReplyServerFailure
		}
	}
	return message.OperationReplyServerFailure
}

func convertICMPError(msg *icmp.Message, ip *net.IPAddr, ver int,
) (message.UDPErrorType, *message.SocksAddr, []byte) {
	var code message.UDPErrorType = 0
	var reporter *message.SocksAddr
	// map icmp message to socks6 addresses and code
	hdr := []byte{}

	switch ver {
	case 4:
		reporter = &message.SocksAddr{
			AddressType: message.AddressTypeIPv4,
			Address:     ip.IP.To4(),
		}

		switch msg.Type {
		case ipv4.ICMPTypeDestinationUnreachable:
			switch msg.Code {
			case 0:
				code = message.UDPErrorNetworkUnreachable
			case 1:
				code = message.UDPErrorHostUnreachable
			default:
				return 0, nil, nil
			}
			m2 := msg.Body.(*icmp.DstUnreach)
			hdr = m2.Data
		case ipv4.ICMPTypeTimeExceeded:
			switch msg.Code {
			case 0:
				code = message.UDPErrorTTLExpired
			default:
				return 0, nil, nil
			}
			m2 := msg.Body.(*icmp.TimeExceeded)
			hdr = m2.Data
		}
	case 6:
		reporter = &message.SocksAddr{
			AddressType: message.AddressTypeIPv6,
			Address:     ip.IP.To16(),
		}

		switch msg.Type {
		case ipv6.ICMPTypeDestinationUnreachable:
			switch msg.Code {
			case 0:
				code = message.UDPErrorNetworkUnreachable
			case 3:
				code = message.UDPErrorHostUnreachable
			default:
				return 0, nil, nil
			}
			m2 := msg.Body.(*icmp.DstUnreach)
			hdr = m2.Data
		case ipv6.ICMPTypeTimeExceeded:
			switch msg.Code {
			case 0:
				code = message.UDPErrorTTLExpired
			default:
				return 0, nil, nil
			}
			m2 := msg.Body.(*icmp.TimeExceeded)
			hdr = m2.Data
		case ipv6.ICMPTypePacketTooBig:
			code = message.UDPErrorDatagramTooBig
			m2 := msg.Body.(*icmp.TimeExceeded)
			hdr = m2.Data
		}
	}
	return code, reporter, hdr
}
