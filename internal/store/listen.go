package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	QueueChannel = "doorstep_queue"

	listenMinBackoff   = time.Second
	listenMaxBackoff   = 30 * time.Second
	listenCloseTimeout = 5 * time.Second
)

func (s *Store) ListenQueue(ctx context.Context, wake func(), logger *slog.Logger) error {
	if s.pool == nil {
		return fmt.Errorf("listen: %w", errInTransaction)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	backoff := listenMinBackoff
	for {
		err := s.listenOnce(ctx, wake, func() { backoff = listenMinBackoff })
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.WarnContext(ctx, "queue listener disconnected",
			slog.String("error", err.Error()),
			slog.Duration("retry_in", backoff),
		)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, listenMaxBackoff)
	}
}

func (s *Store) listenOnce(ctx context.Context, wake, connected func()) (err error) {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), listenCloseTimeout)
		defer cancel()
		err = errors.Join(err, conn.Close(closeCtx))
	}()

	var schema *string
	if err := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return fmt.Errorf("read current schema: %w", err)
	}
	if schema == nil {
		return errors.New("no current schema: the search_path names no existing schema")
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{QueueChannel}.Sanitize()); err != nil {
		return fmt.Errorf("listen on %s: %w", QueueChannel, err)
	}
	connected()
	wake()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}
		if n.Payload == *schema {
			wake()
		}
	}
}
