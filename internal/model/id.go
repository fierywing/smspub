package model

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败仅可能出现在系统层面，此时退化为固定前缀也可接受
		return "id0000000000000000"
	}
	return hex.EncodeToString(b)
}
