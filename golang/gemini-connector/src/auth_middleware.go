package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

func validBearerToken(authorization string, expectedHash [32]byte) bool {
	var token string
	if strings.HasPrefix(authorization, "Bearer ") {
		token = strings.TrimPrefix(authorization, "Bearer ")
	}
	actualHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) == 1
}
