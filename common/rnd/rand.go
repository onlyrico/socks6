package rnd

import (
	"crypto/rand"
	"encoding/binary"

	"github.com/samber/lo"
)

func RandUint16() uint16 {
	buf := RandBytes(2)
	return binary.BigEndian.Uint16(buf)
}
func RandUint32() uint32 {
	buf := RandBytes(4)
	return binary.BigEndian.Uint32(buf)
}
func RandUint64() uint64 {
	buf := RandBytes(8)
	return binary.BigEndian.Uint64(buf)
}

func RandBytes(l int) []byte {
	r := make([]byte, l)
	lo.Must1(rand.Read(r))
	return r
}
