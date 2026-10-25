package main

import "context"

type validTimeContextKey struct{}

func withRequestedValidTime(ctx context.Context, asOf string) context.Context {
	if asOf == "" {
		return ctx
	}
	return context.WithValue(ctx, validTimeContextKey{}, asOf)
}

func temporalScopeFor(asOf string) string {
	if asOf != "" {
		return "current_system_belief_valid_at"
	}
	return ""
}
