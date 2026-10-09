package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	env "ovk-im/src/config"
	"ovk-im/src/db"
	migrate "ovk-im/src/db/migrate"
	"ovk-im/src/redis"
	redisrepo "ovk-im/src/repo/redis"
	"ovk-im/src/repo/search"
	"ovk-im/src/transport/broadcaster"
	"ovk-im/src/transport/endpoints"
	"ovk-im/src/transport/endpoints/core"
	"ovk-im/src/transport/endpoints/status"
	lp_trans "ovk-im/src/transport/longpoll"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] == "start" {
		startServer()
		return
	}

	switch os.Args[1] {
	case "db":
		if len(os.Args) < 3 {
			log.Fatal("Usage: db [create|migrate-legacy]")
		}

		db.Connect()

		switch os.Args[2] {
		case "create":
			migrate.CreateOrMigrateDB()
		case "migrate-legacy":
			migrate.MigrateFromLegacy()
		default:
			log.Fatalf("Unknown db command: %s", os.Args[2])
		}

	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		fmt.Println("Usage: [start | db create | db migrate-legacy]")
	}
}

func startServer() {
	log.Println("Starting OpenVK-IM server...")

	secret := env.Get("SECRET_KEY", "aaa")

	if len(secret) < 64 {
		log.Println("SECRET_KEY .env variable is less than 64 bytes")
		return
	}

	db.Connect()
	redis.Init()

	searchRepo := search.NewRepository(db.Instance, []byte(secret))
	lpBroadcaster := broadcaster.New()
	lpRepo := redisrepo.NewRepo(redis.Client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		pubsub := redis.Client.Subscribe(ctx, "lp_updates")
		defer pubsub.Close()

		for msg := range pubsub.Channel() {
			userID, _ := strconv.ParseInt(msg.Payload, 10, 64)
			lpBroadcaster.Notify(userID)
		}
	}()

	status.StartOnlineTracker(ctx, db.Instance, redis.Client, lpRepo, lpBroadcaster)

	if !env.IsDev() {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/nim", func(c *gin.Context) {
		lp_trans.LongPollHandler(c.Writer, c.Request, lpBroadcaster, lpRepo)
	})

	r.POST("/nim", func(c *gin.Context) {
		if err := c.Request.ParseForm(); err == nil && len(c.Request.PostForm) > 0 {
			if c.Request.URL.RawQuery == "" {
				c.Request.URL.RawQuery = c.Request.PostForm.Encode()
			} else {
				c.Request.URL.RawQuery = c.Request.URL.RawQuery + "&" + c.Request.PostForm.Encode()
			}
		}
		lp_trans.LongPollHandler(c.Writer, c.Request, lpBroadcaster, lpRepo)
	})

	// Superapp queue channel (account-settings/presence realtime). The client
	// long-polls it expecting a {"OK":1} keep-alive; hold for the requested wait
	// window and answer OK so the connection stays alive without busy-looping.
	queueHandler := func(c *gin.Context) {
		wait := 25
		if w := c.Query("wait"); w != "" {
			if n, err := strconv.Atoi(w); err == nil && n > 0 {
				wait = n
			}
		}
		if wait > 30 {
			wait = 30
		}
		select {
		case <-time.After(time.Duration(wait) * time.Second):
		case <-c.Request.Context().Done():
		}
		c.JSON(http.StatusOK, gin.H{"OK": 1})
	}
	r.GET("/queue", queueHandler)
	r.POST("/queue", queueHandler)

	endpointRouter := &endpoints.Router{
		BaseHandler: core.BaseHandler{
			DB:          db.Instance,
			LPRepo:      lpRepo,
			Broadcaster: lpBroadcaster,
			SearchRepo:  searchRepo,
		},
	}
	internal := r.Group("/method")
	{
		endpointRouter.Register(internal)
	}

	port := env.Get("APP_PORT", "8080")
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	log.Printf("Server started on port %s", port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	sqlDB, _ := db.Instance.DB()
	sqlDB.Close()
	redis.Client.Close()
}
