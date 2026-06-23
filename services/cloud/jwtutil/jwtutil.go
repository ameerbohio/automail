// Package jwtutil issues and verifies the cloud server's RS256 access
// tokens. Shared between the JWT middleware (package main) and the auth
// handlers (package handlers) so token format/claims live in one place.
// This keypair is unrelated to the mTLS PKI and the printer's document
// keypair -- see docs/study/03-jwt-rs256-vs-hs256.md.
package jwtutil

import (
	"crypto/rsa"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const AccessTokenTTL = 15 * time.Minute

type AccessClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

func IssueAccessToken(privKey *rsa.PrivateKey, senderID uuid.UUID, role string) (string, error) {
	claims := AccessClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   senderID.String(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(AccessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(privKey)
}

func ParseAccessToken(pubKey *rsa.PublicKey, raw string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		return pubKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		return nil, err
	}
	return claims, nil
}
