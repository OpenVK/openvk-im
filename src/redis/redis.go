package redis

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	env "ovk-im/src/config"

	"github.com/redis/go-redis/v9"
)

var (
	Client *redis.Client
	Ctx    = context.Background()
)

func Init() {
	host := env.Get("REDIS_HOST", "127.0.0.1")
	port := env.Get("REDIS_PORT", "6379")
	pass := env.Get("REDIS_PASS", "")
	db := env.Get("REDIS_DB", "0")
	socket := strings.TrimSpace(env.Get("REDIS_SOCKET", ""))
	dbn, _ := strconv.Atoi(db)

	opts := &redis.Options{
		Password: pass,
		DB:       dbn,
		PoolSize: 50,
	}

	if socket != "" {
		opts.Network = "unix"
		opts.Addr = socket
	} else {
		opts.Network = "tcp"
		opts.Addr = fmt.Sprintf("%s:%s", host, port)
	}

	Client = redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(Ctx, 5*time.Second)
	defer cancel()

	if _, err := Client.Ping(ctx).Result(); err != nil {
		log.Fatalf("Redis connection failed: %v", err)
	}

	if socket != "" {
		log.Printf("Redis connected via socket '%s' (db: %d)", socket, dbn)
	} else {
		log.Printf("Redis connected to %s:%s (db: %d)", host, port, dbn)
	}
}
