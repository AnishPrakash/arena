// file: internal/api/sse.go
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AnishPrakash/arena/internal/adapters/redisq"
)

func (s *Server) publishEvent(ctx context.Context, idOrTopic string, payload any) {
	topic := idOrTopic
	if len(idOrTopic) == 36 { // a bare submission UUID
		topic = redisq.TopicSubmission(idOrTopic)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = s.bus.Publish(ctx, topic, b)
}

// handleSubmissionSSE streams status changes for one submission.
//
// Why SSE and not WebSockets: the data flows one way, SSE is plain HTTP (so it traverses
// proxies and load balancers without an upgrade handshake), and browsers reconnect
// automatically. The cost of a WebSocket here would be bidirectional machinery nobody uses.
//
// Why it is backed by Redis pub/sub: the SSE connection is held by ONE API pod but the
// verdict arrives at whichever pod the runner posted to. The bus makes every pod able to
// serve every stream, which is what keeps the API layer horizontally scalable.
func (s *Server) handleSubmissionSSE(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	id := chi.URLParam(r, "id")

	sub, err := s.store.GetSubmission(ctx, id)
	if err != nil {
		writeErr(w, 404, "submission not found")
		return
	}
	if sub.UserID != u.UserID && u.Role != "admin" {
		writeErr(w, 403, "not your submission")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx buffering the stream
	w.WriteHeader(200)

	// Send current state immediately. A client that connects AFTER the verdict landed
	// must not hang waiting for an event that already happened.
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	send(sub)
	if sub.Status == "DONE" || sub.Status == "FAILED" {
		return
	}

	ch, cancel, err := s.bus.Subscribe(ctx, redisq.TopicSubmission(id))
	if err != nil {
		return
	}
	defer cancel()

	// Hard cap: an SSE connection is a goroutine plus a socket. Without a deadline, a
	// forgotten browser tab holds both forever, and thousands of them are an outage.
	deadline := time.After(5 * time.Minute)
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n") // comment frame keeps proxies from closing the idle connection
			flusher.Flush()
		case msg, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
			var probe struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(msg, &probe) == nil && (probe.Status == "DONE" || probe.Status == "FAILED") {
				return
			}
		}
	}
}
