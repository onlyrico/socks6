package message_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/studentmain/socks6/internal"
	"github.com/studentmain/socks6/message"
)

func TestRequest(t *testing.T) {
	tests := []struct {
		in     []byte
		expect *message.Request
		e      error
	}{
		{
			in: []byte{
				6, 1, 0, 0,
				0, 1, 0, 1,
				127, 0, 0, 1,
			}, expect: &message.Request{
				CommandCode: 1,
				Endpoint:    message.NewAddrMust("127.0.0.1:1"),
				Options:     message.NewOptionSet(),
			}, e: nil,
		},
		{in: []byte{6, 1, 0, 0}, expect: nil, e: io.ErrUnexpectedEOF},
		{in: []byte{6, 1, 0, 0, 0, 0, 0, 1}, expect: nil, e: io.EOF},
		{
			in:     []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 0},
			expect: nil,
			e:      message.ErrVersion{Version: 5, ConsumedBytes: []byte{5}},
		},
		{
			in: []byte{
				6, 1, 0, 4,
				0, 1, 0, 1,
				127, 0, 0, 1,
				1, 0, 0, 4,
			}, expect: &message.Request{
				CommandCode: 1,
				Endpoint:    message.NewAddrMust("127.0.0.1:1"),
				Options:     internal.Must2(message.ParseOptionSetFrom(bytes.NewReader([]byte{1, 0, 0, 4}), 4)).(*message.OptionSet),
			}, e: nil,
		}, {
			in: []byte{
				6, 1, 0, 4,
				0, 1, 0, 1,
				127, 0, 0, 1,
			}, expect: nil, e: io.EOF,
		},
	}

	for _, tt := range tests {
		actual, err := message.ParseRequestFrom(bytes.NewReader(tt.in))
		if tt.e != nil {
			assert.Equal(t, err, tt.e)
		} else {
			assert.Nil(t, err)
			assert.Equal(t, tt.expect, actual)
			assert.Equal(t, tt.in, tt.expect.Marshal())
		}
	}
}
