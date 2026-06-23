// Package authctx is the narrow shared surface between the JWT middleware
// (package main, middleware.go) and the handlers package: a context key
// both sides agree on, so handlers can read the sender identity middleware
// already extracted without either side importing the other's JWT code.
package authctx

import (
	"context"

	"github.com/google/uuid"
)

type ctxKey string

const senderIDKey ctxKey = "sender_id"
const senderRoleKey ctxKey = "sender_role"

func WithSender(ctx context.Context, senderID, role string) context.Context {
	ctx = context.WithValue(ctx, senderIDKey, senderID)
	return context.WithValue(ctx, senderRoleKey, role)
}

// SenderID returns the authenticated sender's ID, or ok=false for a guest
// (no Bearer token, or optionalAuth couldn't validate one).
func SenderID(ctx context.Context) (uuid.UUID, bool) {
	raw, ok := ctx.Value(senderIDKey).(string)
	if !ok {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
}

func Role(ctx context.Context) string {
	role, _ := ctx.Value(senderRoleKey).(string)
	return role
}
