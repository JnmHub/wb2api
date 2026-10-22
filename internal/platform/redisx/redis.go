// Package redisx owns the optional Redis connection. Redis is used for
// cross-process concurrency/scheduler state; the gateway still works without it
// so the file/SQLite deployment remains available.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client is the shared Redis handle.
type Client struct {
	*redis.Client
}

// Open connects and pings Redis.
func Open(ctx context.Context, addr, password string, db int) (*Client, error) {
	if addr == "" {
		return nil, errors.New("empty redis addr")
	}
	c := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolSize:     256,
		MinIdleConns: 8,
		// Renewal and cleanup deadlines must bound socket I/O, not just
		// connection-pool waits and retries.
		ContextTimeoutEnabled: true,
	})
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Client{Client: c}, nil
}

// Ping is used by /health.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.Client == nil {
		return errors.New("redis not configured")
	}
	return c.Client.Ping(ctx).Err()
}
