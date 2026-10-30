package joystick

import (
	"crypto/rand"
	"encoding/base64"
	"math/big"
)

// mintGuestToken builds the playback token Joystick's web player mints for a
// signed-out viewer: "gtx_" + base64("<base36>_<6 upper-hex>"). The streaming
// server reads it only as a session id, so it carries no secret and lets a
// public stream be recorded without a login.
func mintGuestToken() string {
	return "gtx_" + base64.StdEncoding.EncodeToString([]byte(randBase36(11)+"_"+randHex6()))
}

const base36 = "abcdefghijklmnopqrstuvwxyz0123456789"

func randBase36(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = base36[randInt(len(base36))]
	}
	return string(b)
}

func randHex6() string {
	const hexUpper = "0123456789ABCDEF"
	b := make([]byte, 6)
	for i := range b {
		b[i] = hexUpper[randInt(len(hexUpper))]
	}
	return string(b)
}

func randInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}
