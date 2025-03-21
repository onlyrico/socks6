package socks6

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/studentmain/socks6/auth"
	"github.com/studentmain/socks6/common"
	"github.com/studentmain/socks6/common/lg"
	"github.com/studentmain/socks6/common/nt"
	"github.com/studentmain/socks6/internal/socket"
	"github.com/studentmain/socks6/message"
	"golang.org/x/net/icmp"
)

type CommandHandler func(
	ctx context.Context,
	cc SocksConn,
)

// todo socket like api?

// ServerWorker is a customizeable SOCKS 6 server
type ServerWorker struct {
	Authenticator auth.ServerAuthenticator
	Rule          func(cc SocksConn) bool

	CommandHandlers map[message.CommandCode]CommandHandler
	// VersionErrorHandler will handle non-SOCKS6 protocol request.
	// VersionErrorHandler should close connection by itself
	VersionErrorHandler func(ctx context.Context, ver message.ErrVersionMismatch, conn net.Conn)

	DatagramVersionErrorHandler func(ctx context.Context, ver message.ErrVersionMismatch, dgram nt.Datagram)

	Outbound ServerOutbound

	// control UDP NAT filtering behavior,
	// mapping behavior is always Endpoint Independent.
	//
	// when false, use Endpoint Independent filtering (Full Cone)
	//
	// when true, use Address Dependent filtering (Restricted Cone)
	AddressDependentFiltering bool

	// require request message fully received in first packet
	//
	// Yes, TCP has no "packet" -- but that's only makes sense for people
	// who never need to touch the dark side of Internet.
	// Packet are everywhere in a packet switched network,
	// you can create a stream on it and hide it behind API,
	// but it's still a packet sequence on wire.
	IgnoreFragmentedRequest bool
	EnableICMP              bool

	backlogWorker   common.SyncMap[string, *backlogBindWorker] // map[string]*bl
	reservedUdpAddr common.SyncMap[string, uint64]             // map[string]uint64
	udpAssociation  common.SyncMap[uint64, *udpAssociation]    // map[uint64]*ua
}

// ServerOutbound is a group of function called by ServerWorker when a connection or listener is needed to fullfill client request
type ServerOutbound interface {
	Dial(ctx context.Context, option message.StackOptionInfo, addr *message.SocksAddr) (net.Conn, message.StackOptionInfo, error)
	Listen(ctx context.Context, option message.StackOptionInfo, addr *message.SocksAddr) (net.Listener, message.StackOptionInfo, error)
	ListenPacket(ctx context.Context, option message.StackOptionInfo, addr *message.SocksAddr) (net.PacketConn, message.StackOptionInfo, error)
}

// InternetServerOutbound implements ServerOutbound, create a internet connection/listener
type InternetServerOutbound struct {
	DefaultIPv4        net.IP         // address used when udp association request didn't provide an address
	DefaultIPv6        net.IP         // address used when udp association request didn't provide an address
	MulticastInterface *net.Interface // address
}

func (i InternetServerOutbound) Dial(ctx context.Context, option message.StackOptionInfo, addr *message.SocksAddr) (net.Conn, message.StackOptionInfo, error) {
	return socket.DialWithOption(ctx, *addr, option)
}
func (i InternetServerOutbound) Listen(ctx context.Context, option message.StackOptionInfo, addr *message.SocksAddr) (net.Listener, message.StackOptionInfo, error) {
	return socket.ListenerWithOption(ctx, *addr, option)
}
func (i InternetServerOutbound) ListenPacket(ctx context.Context, option message.StackOptionInfo, addr *message.SocksAddr) (net.PacketConn, message.StackOptionInfo, error) {
	mcast := false
	if addr.AddressType != message.AddressTypeDomainName {
		ip := net.IP(addr.Address)
		if ip.IsMulticast() {
			mcast = true
		} else if ip.IsUnspecified() {
			if addr.AddressType == message.AddressTypeIPv4 {
				addr.Address = i.DefaultIPv4
			} else {
				addr.Address = i.DefaultIPv6
			}
		}
	} else {
		return nil, nil, message.ErrAddressTypeNotSupport
	}
	ua, err := net.ResolveUDPAddr("udp", addr.String())
	if err != nil {
		return nil, nil, err
	}
	if mcast {
		p, err2 := net.ListenMulticastUDP("udp", i.MulticastInterface, ua)
		return p, message.StackOptionInfo{}, err2
	}
	// todo what's going on? why 0.0.0.0 not work?
	p, err := net.ListenUDP("udp", ua)
	return p, message.StackOptionInfo{}, err
}

// NewServerWorker create a standard SOCKS 6 server
func NewServerWorker() *ServerWorker {
	defaultAuth := auth.NewServerAuthenticator()
	defaultAuth.AddMethod(auth.NoneServerAuthenticationMethod{})

	r := &ServerWorker{
		VersionErrorHandler: ReplyVersionSpecificError,
		Authenticator:       defaultAuth,
		Outbound: InternetServerOutbound{
			DefaultIPv4: nt.GuessDefaultIPv4(),
			DefaultIPv6: nt.GuessDefaultIPv6(),
		},
		backlogWorker:   common.NewSyncMap[string, *backlogBindWorker](),
		reservedUdpAddr: common.NewSyncMap[string, uint64](),
		udpAssociation:  common.NewSyncMap[uint64, *udpAssociation](),
	}

	r.CommandHandlers = map[message.CommandCode]CommandHandler{
		message.CommandNoop:         r.NoopHandler,
		message.CommandConnect:      r.ConnectHandler,
		message.CommandBind:         r.BindHandler,
		message.CommandUdpAssociate: r.UdpAssociateHandler,
	}

	return r
}

var notHttpProxyMsg = "This is a SOCKS 6 proxy, not a HTTP proxy"

var httpDoc = strings.Join([]string{
	"<!DOCTYPE html>",
	"<html><head>",
	"<title>500 Internal Server Error</title>",
	"</head><body>",
	"<h1>500 Internal Server Error</h1>",
	"<p>" + notHttpProxyMsg + "</p>",
	"</body></html>",
}, "\r\n")

var httpReply = strings.Join([]string{
	"HTTP/1.0 500 Internal Server Error",
	// technically we should identify deployment (e.g. Contoso firewall proxy, proxy.example.com ...)
	// but how can we know that? hostname?
	"Proxy-Status: SOCKS6Server; error=proxy_configuration_error; details=\"" + notHttpProxyMsg + "\"",
	"Content-Type: text/html",
	"Content-Length: " + fmt.Sprintf("%d", len(httpDoc)),
	"Connection: close",
	"",
	httpDoc,
}, "\r\n")

// ReplyVersionSpecificError guess which protocol client is using, reply corresponding "version error", then close conn
func ReplyVersionSpecificError(ctx context.Context, ver message.ErrVersionMismatch, conn net.Conn) {
	defer conn.Close()
	switch ver.Version {
	// socks4
	case 4:
		// header v0, reply 91
		conn.Write([]byte{0, 91})
	case 5:
		// no method allowed
		conn.Write([]byte{5, 0xff})
	case 6:
		// in case this function is used with a socks5 server
		conn.Write([]byte{6})
	case 'c', 'C', 'd', 'D', 'g', 'G', 'h', 'H', 'o', 'O', 'p', 'P', 't', 'T':
		conn.Write([]byte(httpReply))
	default:
		conn.Write([]byte{6})
	}
}

// ServeStream process incoming TCP and TLS connection
// return when connection process complete, e.g. remote closed connection
func (s *ServerWorker) ServeStream(
	ctx context.Context,
	conn net.Conn,
) {
	cc, cmd, ar := s.handshakeStream(ctx, conn, nil)
	if ar == nil || cc == nil || !ar.Success {
		conn.Close()
		return
	}
	defer s.Authenticator.SessionConnClose(ar.SessionID)
	s.CommandHandlers[cmd](ctx, *cc)
}

// handshakeStream process handshake stage,
// i.e. between client request and server auth reply
func (s *ServerWorker) handshakeStream(
	ctx context.Context,
	conn net.Conn,
	prevAuth *auth.ServerAuthenticationResult,
) (sc *SocksConn, cmd message.CommandCode, authr *auth.ServerAuthenticationResult) {
	closeConn := common.NewCancellableDefer(func() {
		conn.Close()
	})
	defer closeConn.Defer()

	ccid := conn3Tuple(conn)

	lg.Trace(ccid, "start processing")
	// create a wrapper reader if necessary
	var conn1 io.Reader = conn
	if s.IgnoreFragmentedRequest && prevAuth != nil {
		lg.Debug("ignore fragmented request")
		conn1 = &nt.NetBufferOnlyReader{Conn: conn}
	}

	req, err := message.ParseRequestFrom(conn1)
	if err != nil {
		closeConn.Cancel()
		s.handleRequestError(ctx, conn, err)
		return nil, 0, nil
	}
	lg.Tracef("%s requested command %d, %s", ccid, req.CommandCode, req.Endpoint)
	lg.Debugf("%s requested %+v", ccid, req)

	var initData []byte
	if am, ok := req.Options.GetData(message.OptionKindAuthenticationMethodAdvertisement); ok {
		initDataLen := int(am.(message.AuthenticationMethodAdvertisementOptionData).InitialDataLength)
		initData = make([]byte, initDataLen)
		if _, err = io.ReadFull(conn, initData); err != nil {
			lg.Warningf("%s can't read %d bytes initdata: %s", ccid, initDataLen, err)
			return nil, 0, nil
		}
	}

	authResult := prevAuth
	if prevAuth == nil {
		authr2 := s.authn(ctx, conn, req)
		authResult = authr2
		if authResult == nil {
			return nil, 0, nil
		}
		if !authResult.Success {
			lg.Info(ccid, "authenticate fail")
			return nil, 0, nil
		}
		lg.Trace(ccid, "authenticate success")
	} else {
		lg.Debug("authn skipped")
	}

	cc := SocksConn{
		Conn:        conn,
		Request:     req,
		ClientId:    authResult.ClientName,
		Session:     authResult.SessionID,
		InitialData: initData,
	}

	if sid, ok := req.Options.GetData(message.OptionKindStreamID); ok {
		sidVal := sid.(message.StreamIDOptionData).ID
		cc.StreamId = sidVal
	}
	if s.Rule != nil && !s.Rule(cc) {
		lg.Info(ccid, "not allowed by rule")
		conn.Write(message.NewOperationReplyWithCode(message.OperationReplyNotAllowedByRule).Marshal())
		return nil, req.CommandCode, authResult
	}

	// per-command
	_, ok := s.CommandHandlers[req.CommandCode]
	if !ok {
		lg.Warning(ccid, "command not supported", req.CommandCode)
		conn.Write(message.NewOperationReplyWithCode(message.OperationReplyCommandNotSupported).Marshal())
		return nil, req.CommandCode, authResult
	}
	lg.Trace(ccid, "start command specific process", req.CommandCode)

	// it's handler's job to close conn
	closeConn.Cancel()
	return &cc, req.CommandCode, authResult
}

func (s *ServerWorker) handleRequestError(
	ctx context.Context,
	conn net.Conn,
	err error,
) {
	evm := message.ErrVersionMismatch{}
	if errors.As(err, &evm) {
		s.VersionErrorHandler(ctx, evm, conn)
		return
	}
	defer conn.Close()
	// detect and reply addr not support early, as auth can't continue
	if errors.Is(err, message.ErrAddressTypeNotSupport) {
		lg.Debugf("%s atyp not supported, fire and forget error reply", conn3Tuple(conn))

		// todo really failed? need clarify. no addr type = no message border info = can't authn at all
		conn.Write(message.NewAuthenticationReplyWithType(message.AuthenticationReplyFail).Marshal())
		conn.Write(message.NewOperationReplyWithCode(message.OperationReplyAddressNotSupported).Marshal())
		return
	} else {
		lg.Warning(conn3Tuple(conn), "can't parse request", err)
		return
	}
}

func (s *ServerWorker) authn(
	ctx context.Context,
	conn net.Conn,
	req *message.Request,
) *auth.ServerAuthenticationResult {
	ccid := conn3Tuple(conn)
	result1, sac := s.Authenticator.Authenticate(ctx, conn, *req)

	auth := *result1
	if result1.Success {
		// one stage auth, success
		auth = *result1
		reply := setAuthMethodInfo(message.NewAuthenticationReplyWithType(message.AuthenticationReplySuccess), *result1)
		lg.Debugf("%s authenticate %+v, %+v", ccid, auth, reply)
		if _, err := conn.Write(reply.Marshal()); err != nil {
			lg.Warning(ccid, "can't write auth reply", err)
			return nil
		}
	} else if !result1.Continue {
		// one stage auth, can't continue
		reply := message.NewAuthenticationReplyWithType(message.AuthenticationReplyFail)
		if _, err := conn.Write(reply.Marshal()); err != nil {
			lg.Warning(ccid, "can't write reply", err)
			return nil
		}
	} else {
		// two stage auth
		reply1 := setAuthMethodInfo(message.NewAuthenticationReplyWithType(message.AuthenticationReplyFail), *result1)
		if _, err := conn.Write(reply1.Marshal()); err != nil {
			lg.Warning(ccid, "can't write auth reply 1", err)
			return nil
		}
		// run stage 2
		lg.Debug(ccid, "auth stage 2")

		result2, err := s.Authenticator.ContinueAuthenticate(sac, *req)
		if err != nil {
			lg.Warning(ccid, "auth stage 2 error", err)
			conn.Write(message.NewAuthenticationReplyWithType(message.AuthenticationReplyFail).Marshal())
			return nil
		}
		auth = *result2
		reply := setAuthMethodInfo(message.NewAuthenticationReply(), *result2)
		if result2.Success {
			reply.Type = message.AuthenticationReplySuccess
		} else {
			reply.Type = message.AuthenticationReplyFail
		}
		lg.Debugf("%s auth stage 2 done %+v , %+v", ccid, auth, reply)
		if _, err = conn.Write(reply.Marshal()); err != nil {
			lg.Warning(ccid, "can't write auth reply 2", err)
			return nil
		}
	}
	return &auth
}

func (s *ServerWorker) ServeSeqPacket(
	ctx context.Context,
	dgramSrc nt.SeqPacket,
) {
	d0, err := dgramSrc.NextDatagram()
	if err != nil {
		lg.Warning("serve seqpacket first datagram", err)
		return
	}
	assoc, h := s.handleFirstDatagram(ctx, d0)
	assoc.handleUdpUp(ctx, socksDatagram{
		msg:    h,
		src:    d0.RemoteAddr(),
		freply: d0.Reply,
	})

	for {
		d, err := dgramSrc.NextDatagram()
		if err != nil {
			lg.Warning("serve seqpacket datagram", err)
			return
		}
		h, err := message.ParseUDPMessageFrom(bytes.NewReader(d.Data()))
		if err != nil {
			lg.Warning(err)
			return
		}
		assoc.handleUdpUp(ctx, socksDatagram{
			msg:    h,
			src:    d.RemoteAddr(),
			freply: d.Reply,
		})
	}
}

func (s *ServerWorker) ServeDatagram(
	ctx context.Context,
	dgram nt.Datagram,
) {
	assoc, h := s.handleFirstDatagram(ctx, dgram)
	assoc.handleUdpUp(ctx, socksDatagram{
		msg:    h,
		src:    dgram.RemoteAddr(),
		freply: dgram.Reply,
	})
}

func (s *ServerWorker) handleFirstDatagram(
	ctx context.Context,
	dgram nt.Datagram,
) (*udpAssociation, *message.UDPMessage) {
	h, err := message.ParseUDPMessageFrom(bytes.NewReader(dgram.Data()))
	if err != nil {
		evm := message.ErrVersionMismatch{}
		if errors.As(err, &evm) && s.DatagramVersionErrorHandler != nil {
			s.DatagramVersionErrorHandler(ctx, evm, dgram)
		}
		return nil, nil
	}
	assoc, ok := s.udpAssociation.Load(h.AssociationID)
	if !ok {
		return nil, nil
	}
	return assoc, h
}

func (s *ServerWorker) ForwardICMP(ctx context.Context, msg *icmp.Message, ip *net.IPAddr, ver int) {
	code, reporter, hdr := convertICMPError(msg, ip, ver)
	if hdr == nil {
		return
	}
	ipSrc, ipDst, proto, err := nt.ParseSrcDstAddrFromIPHeader(hdr, ver)
	if err != nil {
		lg.Info("ICMP IP header parse fail", err)
		return
	}
	if proto != 17 {
		return
	}
	// todo faster way to find corresponding assoc
	s.udpAssociation.Range(func(key uint64, value *udpAssociation) bool {
		ua := value
		// icmp disabled
		if !ua.icmpOn {
			return true
		}
		// not same origin
		if ua.udp.LocalAddr().String() != ipSrc.String() {
			return true
		}
		ua.handleIcmpDown(ctx, code, ipSrc, ipDst, reporter)
		return true
	})
}

func (s *ServerWorker) ServeMuxConn(
	ctx context.Context,
	mux nt.MultiplexedConn,
) {
	defer mux.Close()
	c0, err := mux.Accept()
	if err != nil {
		return
	}
	sc0, cmd0, auth0 := s.handshakeStream(ctx, c0, nil)
	if auth0 == nil || !auth0.Success {
		return
	}
	defer s.Authenticator.SessionConnClose(auth0.SessionID)
	sc0.MuxConn = mux
	go s.CommandHandlers[cmd0](ctx, *sc0)

	if umux, ok := mux.(nt.SeqPacket); ok {
		go func() {
			for {
				d, err := umux.NextDatagram()
				if err != nil {
					return
				}
				// strict check for udp
				s.ServeDatagram(ctx, d)
			}
		}()
	}
	for {
		c, err := mux.Accept()
		if err != nil {
			return
		}
		go func() {
			// authn skipped
			sc, cmd, _ := s.handshakeStream(ctx, c, auth0)
			sc.MuxConn = mux
			s.CommandHandlers[cmd](ctx, *sc)
		}()
	}
}

// todo request clear resource by resource themselves

// ClearUnusedResource clear no longer used resources (UDP associations, etc.)
// only need to call it once for each ServerWorker
func (s *ServerWorker) ClearUnusedResource(ctx context.Context) {
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	tick := time.NewTicker(1 * time.Minute)

	for {
		select {
		case <-tick.C:
		case <-ctx2.Done():
			return
		}

		s.backlogWorker.Range(func(key string, value *backlogBindWorker) bool {
			bl := value
			if bl.alive {
				return true
			}
			s.backlogWorker.Delete(key)
			return true
		})
		s.udpAssociation.Range(func(key uint64, value *udpAssociation) bool {
			ua := value
			if ua.alive {
				return true
			}
			s.udpAssociation.Delete(key)
			s.reservedUdpAddr.Delete(ua.pair)
			return true
		})
	}
}

func setAuthMethodInfo(arep *message.AuthenticationReply, result auth.ServerAuthenticationResult) *message.AuthenticationReply {
	if result.SelectedMethod != 0 && result.SelectedMethod != 0xff {
		arep.Options.Add(message.Option{
			Kind: message.OptionKindAuthenticationMethodSelection,
			Data: message.AuthenticationMethodSelectionOptionData{
				Method: result.SelectedMethod,
			},
		})
	}
	if result.MethodData != nil {
		arep.Options.Add(message.Option{
			Kind: message.OptionKindAuthenticationData,
			Data: message.AuthenticationDataOptionData{
				Method: result.SelectedMethod,
				Data:   result.MethodData,
			},
		})
	}
	return arep
}
