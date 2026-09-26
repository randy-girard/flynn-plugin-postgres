package main

import (
	"context"
	"net/http"
	"time"
)

func timeoutCtx(r *http.Request) (context.Context, context.CancelFunc) {
	if r == nil || r.Context() == nil {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}
	return context.WithTimeout(r.Context(), 30*time.Second)
}
