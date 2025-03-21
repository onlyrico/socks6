package main

import (
	"context"
	"net"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/samber/lo"
	"github.com/studentmain/socks6"
	"github.com/studentmain/socks6/auth"
	"github.com/studentmain/socks6/cmd/shadowsocks2021"
	"github.com/studentmain/socks6/message"
)

type ssServerAuth struct {
	auth.DefaultServerAuthenticator
}

func (a *ssServerAuth) Authenticate(
	ctx context.Context,
	conn net.Conn,
	req message.Request,
) (
	*auth.ServerAuthenticationResult,
	*auth.ServerAuthenticationChannels,
) {
	// require time window
	if t, ok := req.Options.GetData(shadowsocks2021.OptionKindSSTick); !ok {
		conn.Close()

		return &auth.ServerAuthenticationResult{
			Success:  false,
			Continue: false,
		}, nil
	} else {
		sstod := t.(shadowsocks2021.SSTickOptionData)
		tooYoung := sstod.Time.After(time.Now().Add(1 * time.Minute))
		tooOld := time.Now().After(sstod.Time.Add(1 * time.Minute))
		if tooYoung || tooOld {
			conn.Close()

			return &auth.ServerAuthenticationResult{
				Success:  false,
				Continue: false,
			}, nil
		}
	}
	// require replay protection
	_, token1 := req.Options.GetData(message.OptionKindIdempotenceExpenditure)
	_, token2 := req.Options.GetData(message.OptionKindTokenRequest)
	if !token1 || !token2 {
		conn.Close()

		return &auth.ServerAuthenticationResult{
			Success:  false,
			Continue: false,
		}, nil
	}

	sar, sac := a.DefaultServerAuthenticator.Authenticate(ctx, conn, req)
	if !sar.Success && !sar.Continue {
		conn.Close()
	}
	return sar, sac
}

func main() {
	sw := socks6.NewServerWorker()
	sw.IgnoreFragmentedRequest = true
	sw.AddressDependentFiltering = true
	sw.Authenticator = &ssServerAuth{
		DefaultServerAuthenticator: *auth.NewServerAuthenticator(),
	}
	l, err := net.Listen("tcp", "127.0.0.1:8388")
	if err != nil {
		panic(err)
	}
	lru := lo.Must(lru.New(4096))
	for {
		c, err := l.Accept()
		if err != nil {
			panic(err)
		}
		sc := shadowsocks2021.NewSSConn(c, []byte("123456"), lru)
		go sw.ServeStream(context.Background(), sc)
	}
}
